package agent

import (
	"database/sql"
	"flag"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strconv"
	"testing"
	"time"

	logger "log"

	"github.com/go-martini/martini"
	"github.com/martini-contrib/render"
	"github.com/openark/golib/log"

	"github.com/github/orchestrator/go/config"
	"github.com/github/orchestrator/go/db"
	"github.com/github/orchestrator/go/inst"
	mysqldrv "github.com/go-sql-driver/mysql"
	"github.com/openark/golib/sqlutils"
	"github.com/ory/dockertest"
	. "gopkg.in/check.v1"
)

// before running add following to your /etc/hosts file depending on number of agents you plan to use
// 127.0.0.2 agent1
// 127.0.0.3 agent2
// ...
// 127.0.0.n agentn

// also if you are using Mac OS X, you will need to create the aliases with
// sudo ifconfig lo0 alias 127.0.0.2 up
// sudo ifconfig lo0 alias 127.0.0.3 up
// for each agent

func init() {
	log.SetLevel(log.DEBUG)
}

var testname = flag.String("testname", "TestProcessSeeds", "test names to run")

func Test(t *testing.T) { TestingT(t) }

type AgentTestSuite struct {
	testAgents map[string]*testAgent
	pool       *dockertest.Pool
	containers []*dockertest.Resource
}

var _ = Suite(&AgentTestSuite{})

type testAgent struct {
	agent                 *Agent
	agentSeedStageStatus  *SeedStageState
	agentSeedMetadata     *SeedMetadata
	agentServer           *httptest.Server
	agentMux              *martini.ClassicMartini
	agentMySQLContainerIP string
	agentMySQLContainerID string
}

func (s *AgentTestSuite) SetUpTest(c *C) {
	if len(*testname) > 0 {
		if c.TestName() != fmt.Sprintf("AgentTestSuite.%s", *testname) {
			c.Skip("skipping test due to not matched testname")
		}
	}
	log.Info("Setting up test data")
	var testAgents = make(map[string]*testAgent)
	s.testAgents = testAgents
	pool, err := dockertest.NewPool("")
	if err != nil {
		log.Fatalf("Could not connect to docker: %s", err)
	}
	s.pool = pool

	// pulls an image, creates a container based on it and runs it
	log.Info("Creating docker container for Orchestrator DB")
	resource, err := pool.Run("mysql", "5.7", []string{"MYSQL_ROOT_PASSWORD=secret"})
	if err != nil {
		log.Fatalf("Could not start resource: %s", err)
	}

	s.containers = append(s.containers, resource)

	// configure Orchestrator
	config.Config.MySQLOrchestratorHost = "127.0.0.1"
	port, _ := strconv.Atoi(resource.GetPort("3306/tcp"))
	config.Config.MySQLOrchestratorPort = uint(port)
	config.Config.MySQLOrchestratorUser = "root"
	config.Config.MySQLOrchestratorPassword = "secret"
	config.Config.MySQLOrchestratorDatabase = "orchestrator"
	config.Config.AgentsServerPort = ":3001"
	config.Config.ServeAgentsHttp = true
	config.Config.AuditToSyslog = false
	config.Config.EnableSyslog = false
	config.Config.MySQLConnectTimeoutSeconds = 600
	config.Config.MySQLConnectionLifetimeSeconds = 600
	config.Config.MySQLOrchestratorReadTimeoutSeconds = 600
	config.Config.MySQLTopologyReadTimeoutSeconds = 600
	config.Config.MySQLTopologySSLSkipVerify = true
	config.Config.MySQLTopologyUseMixedTLS = true
	config.Config.HostnameResolveMethod = "none"
	config.Config.MySQLHostnameResolveMethod = "none"
	falseFlag := false
	trueFlag := true
	config.RuntimeCLIFlags.Noop = &falseFlag
	config.RuntimeCLIFlags.SkipUnresolve = &trueFlag
	config.Config.SkipMaxScaleCheck = true
	config.MarkConfigurationLoaded()

	// exponential backoff-retry, because the application in the container might not be ready to accept connections yet
	if err := pool.Retry(func() error {
		var testdb *sql.DB
		var err error
		testdb, _, err = sqlutils.GetDB(fmt.Sprintf("root:secret@(localhost:%s)/mysql", resource.GetPort("3306/tcp")))
		mysqldrv.SetLogger(logger.New(ioutil.Discard, "discard", 1))
		if err != nil {
			return err
		}
		return testdb.Ping()
	}); err != nil {
		log.Fatalf("Could not connect to docker: %s", err)
	}

	// create Orchestrator DB
	log.Info("Creating Orchestrator DB")
	_, err = db.OpenOrchestrator()
	if err != nil {
		log.Fatalf("Unable to create orchestrator DB: %s", err)
	}

	// init few functions nessesary for test process
	inst.InitializeInstanceDao()
	InitHttpClient()
	go func() {
		for seededAgent := range SeededAgents {
			instanceKey := &inst.InstanceKey{Hostname: seededAgent.Info.Hostname, Port: int(seededAgent.Info.MySQLPort)}
			log.Infof("%+v", instanceKey)
		}
	}()

	// create mocks for agents
	log.Info("Creating Orchestrator agents mocks")
	for i := 1; i <= 4; i++ {
		mysqlDatabases := map[string]*MySQLDatabase{
			"sakila": &MySQLDatabase{
				Engines: []Engine{InnoDB},
				Size:    0,
			},
		}
		availiableSeedMethods := map[SeedMethod]*SeedMethodOpts{
			Mydumper: &SeedMethodOpts{
				BackupSide:       Target,
				SupportedEngines: []Engine{ROCKSDB, MRG_MYISAM, CSV, BLACKHOLE, InnoDB, MEMORY, ARCHIVE, MyISAM, FEDERATED, TokuDB},
			},
		}
		agent := &Agent{
			Info: &Info{
				Hostname: fmt.Sprintf("agent%d", i),
				Port:     3002 + i,
				Token:    "token",
			},
			Data: &Data{
				LocalSnapshotsHosts: []string{fmt.Sprintf("127.0.0.%d", i)},
				SnaphostHosts:       []string{fmt.Sprintf("127.0.0.%d", i), "localhost"},
				LogicalVolumes:      []*LogicalVolume{},
				MountPoint: &Mount{
					Path:       "/tmp",
					Device:     "",
					LVPath:     "",
					FileSystem: "",
					IsMounted:  false,
					DiskUsage:  0,
				},
				BackupDir:             "/tmp/bkp",
				BackupDirDiskFree:     10000,
				MySQLRunning:          true,
				MySQLDatadir:          "/var/lib/mysql",
				MySQLDatadirDiskUsed:  10,
				MySQLDatadirDiskFree:  10000,
				MySQLVersion:          "5.7.25",
				MySQLDatabases:        mysqlDatabases,
				AvailiableSeedMethods: availiableSeedMethods,
			},
		}
		s.createTestAgent(agent)
	}
}

func (s *AgentTestSuite) createTestAgent(agent *Agent) {
	agentAddress := fmt.Sprintf("%s:%d", agent.Info.Hostname, agent.Info.Port)
	m := martini.Classic()
	m.Use(render.Renderer())
	testAgent := &testAgent{
		agent:                agent,
		agentSeedStageStatus: &SeedStageState{},
		agentSeedMetadata:    &SeedMetadata{},
	}
	m.Get("/api/get-agent-data", func(r render.Render, res http.ResponseWriter, req *http.Request) {
		r.JSON(200, agent.Data)
	})
	m.Get("/api/prepare/:seedID/:seedMethod/:seedSide", func(r render.Render, res http.ResponseWriter, req *http.Request) {
		r.Text(202, "Started")
	})
	m.Get("/api/backup/:seedID/:seedMethod/:seedHost/:mysqlPort", func(r render.Render, res http.ResponseWriter, req *http.Request) {
		r.Text(202, "Started")
	})
	m.Get("/api/restore/:seedID/:seedMethod", func(r render.Render, res http.ResponseWriter, req *http.Request) {
		r.Text(202, "Started")
	})
	m.Get("/api/cleanup/:seedID/:seedMethod/:seedSide", func(r render.Render, res http.ResponseWriter, req *http.Request) {
		r.Text(202, "Started")
	})
	m.Get("/api/get-metadata/:seedID/:seedMethod", func(r render.Render, res http.ResponseWriter, req *http.Request) {
		r.JSON(200, testAgent.agentSeedMetadata)
	})
	m.Get("/api/seed-stage-state/:seedID/:seedStage", func(r render.Render, res http.ResponseWriter, req *http.Request) {
		r.JSON(200, testAgent.agentSeedStageStatus)
	})
	m.Get("/api/abort-seed-stage/:seedID/:seedStage", func(r render.Render, res http.ResponseWriter, req *http.Request) {
		r.Text(200, "killed")
	})
	testServer := httptest.NewUnstartedServer(m)
	listener, _ := net.Listen("tcp", agentAddress)
	testServer.Listener = listener
	testServer.Start()
	testAgent.agentServer = testServer
	testAgent.agentMux = m
	s.testAgents[agent.Info.Hostname] = testAgent
}

func (s *AgentTestSuite) createTestAgentMySQLServer(agent *testAgent, useGTID bool, createSlaveUser bool) error {
	log.Info("Setting agents MySQL")
	var testdb *sql.DB
	rand.Seed(time.Now().UnixNano())
	serverID := rand.Intn(100000000)

	dockerCmd := []string{"mysqld", fmt.Sprintf("--server-id=%d", serverID), "--log-bin=/var/lib/mysql/mysql-bin"}
	log.Info("Creating docker container for agent")

	if useGTID {
		dockerCmd = append(dockerCmd, "--enforce-gtid-consistency=ON", "--gtid-mode=ON")
	}
	resource, err := s.pool.RunWithOptions(&dockertest.RunOptions{Repository: "mysql", Tag: "5.7", Env: []string{"MYSQL_ROOT_PASSWORD=secret"}, Cmd: dockerCmd, CapAdd: []string{"NET_ADMIN", "NET_RAW"}})
	if err != nil {
		return fmt.Errorf("Could not connect to docker: %s", err)
	}

	agent.agentMySQLContainerIP = resource.Container.NetworkSettings.Networks["bridge"].IPAddress
	agent.agentMySQLContainerID = resource.Container.ID

	s.containers = append(s.containers, resource)

	cmd := exec.Command("docker", "exec", "-i", agent.agentMySQLContainerID, "apt-get", "update")
	if err := cmd.Run(); err != nil {
		return err
	}
	cmd = exec.Command("docker", "exec", "-i", agent.agentMySQLContainerID, "apt-get", "install", "-y", "iptables")
	if err := cmd.Run(); err != nil {
		return err
	}

	if err := s.pool.Retry(func() error {
		var err error
		testdb, _, err = sqlutils.GetDB(fmt.Sprintf("root:secret@(localhost:%s)/mysql", resource.GetPort("3306/tcp")))
		mysqldrv.SetLogger(logger.New(ioutil.Discard, "discard", 1))
		if err != nil {
			return err
		}
		return testdb.Ping()
	}); err != nil {
		return fmt.Errorf("Could not connect to docker: %s", err)
	}

	if createSlaveUser {
		if _, err = testdb.Exec("Create database orchestrator_meta;"); err != nil {
			return err
		}
		if _, err = testdb.Exec("Create table orchestrator_meta.replication (`username` varchar(128) CHARACTER SET ascii NOT NULL DEFAULT '',`password` varchar(128) CHARACTER SET ascii NOT NULL DEFAULT '',PRIMARY KEY (`username`,`password`)) ENGINE=InnoDB DEFAULT CHARSET=utf8;"); err != nil {
			return err
		}
		if _, err = testdb.Exec("CREATE USER `slave`@`%` IDENTIFIED BY 'slavepassword@';"); err != nil {
			return err
		}
		if _, err = testdb.Exec("GRANT REPLICATION SLAVE ON *.* TO `slave`@`%`"); err != nil {
			return err
		}
		if _, err = testdb.Exec("CREATE USER `orc_topology`@`%` IDENTIFIED BY 'orc_topologypassword@';"); err != nil {
			return err
		}
		if _, err = testdb.Exec("GRANT ALL PRIVILEGES ON *.* TO `orc_topology`@`%`"); err != nil {
			return err
		}
		if _, err = testdb.Exec("Insert into orchestrator_meta.replication(username, password) VALUES ('slave', 'slavepassword@');"); err != nil {
			return err
		}
	}

	if _, err = testdb.Exec("Create database test_repl"); err != nil {
		return err
	}
	if _, err = testdb.Exec("Create table test_repl.test(id int);"); err != nil {
		return err
	}
	if _, err = testdb.Exec("insert into test_repl.test(id) VALUES (1), (2), (3), (4);"); err != nil {
		return err
	}

	if err = sqlutils.QueryRowsMap(testdb, "show master status", func(m sqlutils.RowMap) error {
		var err error
		agent.agentSeedMetadata.LogFile = m.GetString("File")
		agent.agentSeedMetadata.LogPos = m.GetInt64("Position")
		agent.agentSeedMetadata.GtidExecuted = m.GetString("Executed_Gtid_Set")
		return err
	}); err != nil {
		return err
	}

	mysqlPort, err := strconv.Atoi(resource.GetPort("3306/tcp"))
	if err != nil {
		return err
	}
	agent.agent.Info.MySQLPort = mysqlPort
	return nil
}

// this is needed in order to redirect from host machine ip 172.0.0.x to container ip for replication to work
func (s *AgentTestSuite) createIPTablesRulesForReplication(targetAgent *testAgent, sourceAgent *testAgent) error {
	cmd := exec.Command("docker", "exec", "-i", targetAgent.agentMySQLContainerID, "iptables", "-t", "nat", "-I", "OUTPUT", "-p", "tcp", "-o", "eth0", "--dport", fmt.Sprintf("%d", sourceAgent.agent.Info.MySQLPort), "-j", "DNAT", "--to-destination", fmt.Sprintf("%s:3306", sourceAgent.agentMySQLContainerIP))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", string(out))
	}
	cmd = exec.Command("docker", "exec", "-i", targetAgent.agentMySQLContainerID, "/bin/sh", "-c", fmt.Sprintf("echo %s %s >> /etc/hosts", sourceAgent.agentMySQLContainerIP, sourceAgent.agent.Info.Hostname))
	out, err = cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", string(out))
	}
	return nil
}

func (s *AgentTestSuite) TearDownTest(c *C) {
	log.Info("Teardown test data")
	for _, container := range s.containers {
		if err := s.pool.Purge(container); err != nil {
			log.Fatalf("Could not purge resource: %s", err)
		}
	}
	for _, testAgent := range s.testAgents {
		testAgent.agentServer.Close()
	}
}

func (s *AgentTestSuite) registerAgents(c *C) {
	for _, testAgent := range s.testAgents {
		hostname, err := RegisterAgent(testAgent.agent.Info)
		c.Assert(err, IsNil)
		c.Assert(hostname, Equals, testAgent.agent.Info.Hostname)
	}
}

func (s *AgentTestSuite) getSeedAgents(c *C, targetTestAgent *testAgent, sourceTestAgent *testAgent) (targetAgent *Agent, sourceAgent *Agent) {
	s.registerAgents(c)
	targetAgent, err := ReadAgent(targetTestAgent.agent.Info.Hostname)
	c.Assert(err, IsNil)

	sourceAgent, err = ReadAgent(sourceTestAgent.agent.Info.Hostname)
	c.Assert(err, IsNil)

	return targetAgent, sourceAgent
}

func (s *AgentTestSuite) readSeed(c *C, seedID int64, targetHostname string, sourceHostname string, seedMethod SeedMethod, backupSide SeedSide, status SeedStatus, stage SeedStage, retries int) *Seed {
	seed, err := ReadSeed(seedID)
	c.Assert(err, IsNil)
	c.Assert(seed.TargetHostname, Equals, targetHostname)
	c.Assert(seed.SourceHostname, Equals, sourceHostname)
	c.Assert(seed.SeedMethod, Equals, seedMethod)
	c.Assert(seed.BackupSide, Equals, backupSide)
	c.Assert(seed.Status, Equals, status)
	c.Assert(seed.Stage, Equals, stage)
	c.Assert(seed.Retries, Equals, retries)
	return seed
}

func (s *AgentTestSuite) readSeedStageStates(c *C, seed *Seed, stateRecords int, targetTestAgent *testAgent, sourceTestAgent *testAgent) {
	seedStates, err := seed.ReadSeedStageStates()
	c.Assert(err, IsNil)
	for _, seedState := range seedStates[:stateRecords] {
		for _, agent := range []*testAgent{targetTestAgent, sourceTestAgent} {
			if seedState.Hostname == agent.agent.Info.Hostname {
				c.Assert(seedState.SeedID, Equals, agent.agentSeedStageStatus.SeedID)
				c.Assert(seedState.Stage, Equals, agent.agentSeedStageStatus.Stage)
				c.Assert(seedState.Status, Equals, agent.agentSeedStageStatus.Status)
			}
		}
	}
}

func (s *AgentTestSuite) TestAgentRegistration(c *C) {
	s.registerAgents(c)
}

func (s *AgentTestSuite) TestReadAgents(c *C) {
	s.registerAgents(c)

	registeredAgents, err := ReadAgents()
	c.Assert(err, IsNil)
	c.Assert(registeredAgents, HasLen, 4)

	for _, testAgent := range s.testAgents {
		for _, registeredAgent := range registeredAgents {
			if registeredAgent.Info.Port == testAgent.agent.Info.Port && registeredAgent.Info.Hostname == testAgent.agent.Info.Hostname {
				c.Assert(registeredAgent.Info, DeepEquals, testAgent.agent.Info)
				c.Assert(registeredAgent.Data, DeepEquals, testAgent.agent.Data)
			}
		}
	}
}

func (s *AgentTestSuite) TestReadAgentsInfo(c *C) {
	s.registerAgents(c)

	registeredAgents, err := ReadAgentsInfo()
	c.Assert(err, IsNil)
	c.Assert(registeredAgents, HasLen, 4)

	for _, testAgent := range s.testAgents {
		for _, registeredAgent := range registeredAgents {
			if registeredAgent.Info.Port == testAgent.agent.Info.Port && registeredAgent.Info.Hostname == testAgent.agent.Info.Hostname {
				c.Assert(registeredAgent.Info, DeepEquals, testAgent.agent.Info)
				c.Assert(registeredAgent.Data, DeepEquals, &Data{})
			}
		}
	}
}

func (s *AgentTestSuite) TestReadAgent(c *C) {
	testAgent := s.testAgents["agent1"]

	s.registerAgents(c)

	registeredAgent, err := ReadAgent(testAgent.agent.Info.Hostname)
	c.Assert(err, IsNil)
	c.Assert(registeredAgent.Info, DeepEquals, testAgent.agent.Info)
	c.Assert(registeredAgent.Data, DeepEquals, testAgent.agent.Data)
}

func (s *AgentTestSuite) TestReadAgentInfo(c *C) {
	testAgent := s.testAgents["agent1"]

	s.registerAgents(c)

	registeredAgent, err := ReadAgentInfo(testAgent.agent.Info.Hostname)
	c.Assert(err, IsNil)
	c.Assert(registeredAgent.Info, DeepEquals, testAgent.agent.Info)
	c.Assert(registeredAgent.Data, DeepEquals, &Data{})
}

func (s *AgentTestSuite) TestReadOutdatedAgents(c *C) {
	config.Config.AgentPollMinutes = 2

	s.registerAgents(c)

	outdatedAgents := []*testAgent{s.testAgents["agent1"]}
	upToDateAgents := []*testAgent{s.testAgents["agent2"], s.testAgents["agent3"], s.testAgents["agent4"]}

	for _, outdatedAgent := range outdatedAgents {
		db.ExecOrchestrator(fmt.Sprintf("UPDATE host_agent SET last_checked = NOW() - interval 60 minute WHERE hostname='%s'", outdatedAgent.agent.Info.Hostname))
	}

	for _, upToDateAgent := range upToDateAgents {
		db.ExecOrchestrator(fmt.Sprintf("UPDATE host_agent SET last_checked = NOW() WHERE hostname='%s'", upToDateAgent.agent.Info.Hostname))
	}

	outdatedOrchestratorAgents, err := ReadOutdatedAgents()
	c.Assert(err, IsNil)
	c.Assert(outdatedOrchestratorAgents, HasLen, 1)
	c.Assert(outdatedOrchestratorAgents[0].Info, DeepEquals, outdatedAgents[0].agent.Info)
	c.Assert(outdatedOrchestratorAgents[0].Data, DeepEquals, &Data{})
}

func (s *AgentTestSuite) TestUpdateAgent(c *C) {
	testAgent := s.testAgents["agent1"]

	s.registerAgents(c)

	registeredAgent, err := ReadAgentInfo(testAgent.agent.Info.Hostname)
	c.Assert(err, IsNil)

	testAgent.agent.Data.LocalSnapshotsHosts = []string{"127.0.0.10", "127.0.0.12"}
	registeredAgent.Status = Inactive

	err = registeredAgent.updateAgentStatus()
	c.Assert(err, IsNil)

	err = registeredAgent.UpdateAgent()
	c.Assert(err, IsNil)

	c.Assert(registeredAgent.Info, DeepEquals, testAgent.agent.Info)
	c.Assert(registeredAgent.Data, DeepEquals, testAgent.agent.Data)
	c.Assert(registeredAgent.Status, Equals, Active)
}

func (s *AgentTestSuite) TestUpdateAgentFailed(c *C) {
	testAgent := s.testAgents["agent1"]

	s.registerAgents(c)
	testAgent.agentServer.Close()

	registeredAgent, err := ReadAgentInfo(testAgent.agent.Info.Hostname)
	c.Assert(err, IsNil)

	err = registeredAgent.UpdateAgent()
	c.Assert(err, NotNil)

	registeredAgent, err = ReadAgentInfo(testAgent.agent.Info.Hostname)
	c.Assert(err, IsNil)
	c.Assert(registeredAgent.Status, Equals, Inactive)
}

func (s *AgentTestSuite) TestForgetLongUnseenAgents(c *C) {
	config.Config.UnseenAgentForgetHours = 1
	testAgent := s.testAgents["agent1"]

	s.registerAgents(c)
	db.ExecOrchestrator(fmt.Sprintf("UPDATE host_agent SET last_seen = last_seen - interval 2 hour WHERE hostname='%s'", testAgent.agent.Info.Hostname))

	err := ForgetLongUnseenAgents()
	c.Assert(err, IsNil)

	_, err = ReadAgentInfo(testAgent.agent.Info.Hostname)
	c.Assert(err, NotNil)
}

func (s *AgentTestSuite) TestNewSeed(c *C) {
	targetTestAgent := s.testAgents["agent1"]
	sourceTestAgent := s.testAgents["agent2"]

	s.registerAgents(c)
	targetAgent, sourceAgent := s.getSeedAgents(c, targetTestAgent, sourceTestAgent)

	seedID, err := NewSeed("Mydumper", targetAgent, sourceAgent)
	c.Assert(err, IsNil)
	c.Assert(seedID, Equals, int64(1))
}

func (s *AgentTestSuite) TestNewSeedWrongSeedMethod(c *C) {
	targetTestAgent := s.testAgents["agent1"]
	sourceTestAgent := s.testAgents["agent2"]

	s.registerAgents(c)
	targetAgent, sourceAgent := s.getSeedAgents(c, targetTestAgent, sourceTestAgent)

	_, err := NewSeed("test", targetAgent, sourceAgent)
	c.Assert(err, NotNil)
}

func (s *AgentTestSuite) TestNewSeedSeedItself(c *C) {
	targetTestAgent := s.testAgents["agent1"]
	sourceTestAgent := s.testAgents["agent1"]

	s.registerAgents(c)

	targetAgent, sourceAgent := s.getSeedAgents(c, targetTestAgent, sourceTestAgent)

	_, err := NewSeed("Mydumper", targetAgent, sourceAgent)
	c.Assert(err, NotNil)
}

func (s *AgentTestSuite) TestNewSeedUnsupportedSeedMethod(c *C) {
	targetTestAgent := s.testAgents["agent1"]
	sourceTestAgent := s.testAgents["agent2"]

	s.registerAgents(c)
	targetAgent, sourceAgent := s.getSeedAgents(c, targetTestAgent, sourceTestAgent)

	_, err := NewSeed("Mysqldump", targetAgent, sourceAgent)
	c.Assert(err, NotNil)
}

func (s *AgentTestSuite) TestNewSeedUnsupportedSeedMethodForDB(c *C) {
	targetTestAgent := s.testAgents["agent1"]
	sourceTestAgent := s.testAgents["agent2"]

	sourceTestAgent.agent.Data.AvailiableSeedMethods[Xtrabackup] = &SeedMethodOpts{
		BackupSide:       Target,
		SupportedEngines: []Engine{MRG_MYISAM, CSV, BLACKHOLE, InnoDB, MEMORY, ARCHIVE, MyISAM, FEDERATED, TokuDB},
	}
	sourceTestAgent.agent.Data.MySQLDatabases["test"] = &MySQLDatabase{
		Engines: []Engine{ROCKSDB},
		Size:    0,
	}

	s.registerAgents(c)
	targetAgent, sourceAgent := s.getSeedAgents(c, targetTestAgent, sourceTestAgent)

	_, err := NewSeed("Xtrabackup", targetAgent, sourceAgent)
	c.Assert(err, NotNil)
}

func (s *AgentTestSuite) TestNewSeedSourceAgentMySQLVersionLessThanTarget(c *C) {
	targetTestAgent := s.testAgents["agent1"]
	sourceTestAgent := s.testAgents["agent2"]

	targetTestAgent.agent.Data.MySQLVersion = "5.6.40"

	s.registerAgents(c)
	targetAgent, sourceAgent := s.getSeedAgents(c, targetTestAgent, sourceTestAgent)

	_, err := NewSeed("Mydumper", targetAgent, sourceAgent)
	c.Assert(err, NotNil)
}

func (s *AgentTestSuite) TestNewSeedAgentHadActiveSeed(c *C) {
	targetTestAgent := s.testAgents["agent1"]
	sourceTestAgent := s.testAgents["agent2"]

	s.registerAgents(c)
	targetAgent, sourceAgent := s.getSeedAgents(c, targetTestAgent, sourceTestAgent)

	_, err := NewSeed("Mydumper", targetAgent, sourceAgent)
	c.Assert(err, IsNil)

	_, err = NewSeed("Mydumper", targetAgent, sourceAgent)
	c.Assert(err, NotNil)
}

func (s *AgentTestSuite) TestNewSeedNotEnoughSpaceInMySQLDatadir(c *C) {
	targetTestAgent := s.testAgents["agent1"]
	sourceTestAgent := s.testAgents["agent2"]
	targetTestAgent.agent.Data.MySQLDatadirDiskFree = 10
	sourceTestAgent.agent.Data.MySQLDatadirDiskUsed = 1000

	s.registerAgents(c)
	targetAgent, sourceAgent := s.getSeedAgents(c, targetTestAgent, sourceTestAgent)

	_, err := NewSeed("Mydumper", targetAgent, sourceAgent)
	c.Assert(err, NotNil)
}

func (s *AgentTestSuite) TestReadSeed(c *C) {
	targetTestAgent := s.testAgents["agent1"]
	sourceTestAgent := s.testAgents["agent2"]

	s.registerAgents(c)

	targetAgent, sourceAgent := s.getSeedAgents(c, targetTestAgent, sourceTestAgent)

	seedID, err := NewSeed("Mydumper", targetAgent, sourceAgent)
	c.Assert(err, IsNil)
	c.Assert(seedID, Equals, int64(1))

	s.readSeed(c, seedID, targetAgent.Info.Hostname, sourceAgent.Info.Hostname, Mydumper, Target, Started, Prepare, 0)
}

func (s *AgentTestSuite) TestReadActiveSeeds(c *C) {
	targetTestAgent1 := s.testAgents["agent1"]
	sourceTestAgent1 := s.testAgents["agent2"]
	targetTestAgent2 := s.testAgents["agent3"]
	sourceTestAgent2 := s.testAgents["agent4"]

	s.registerAgents(c)

	targetAgent1, sourceAgent1 := s.getSeedAgents(c, targetTestAgent1, sourceTestAgent1)
	targetAgent2, sourceAgent2 := s.getSeedAgents(c, targetTestAgent2, sourceTestAgent2)

	seedID, err := NewSeed("Mydumper", targetAgent1, sourceAgent1)
	c.Assert(err, IsNil)
	c.Assert(seedID, Equals, int64(1))

	seedID, err = NewSeed("Mydumper", targetAgent2, sourceAgent2)
	c.Assert(err, IsNil)
	c.Assert(seedID, Equals, int64(2))

	completedSeed := s.readSeed(c, seedID, targetAgent2.Info.Hostname, sourceAgent2.Info.Hostname, Mydumper, Target, Started, Prepare, 0)
	completedSeed.Status = Completed
	completedSeed.updateSeedData()

	seeds, err := ReadActiveSeeds()
	c.Assert(err, IsNil)
	c.Assert(seeds, HasLen, 1)

	c.Assert(seeds[0].TargetHostname, Equals, targetTestAgent1.agent.Info.Hostname)
	c.Assert(seeds[0].SourceHostname, Equals, sourceTestAgent1.agent.Info.Hostname)
	c.Assert(seeds[0].SeedMethod, Equals, Mydumper)
	c.Assert(seeds[0].BackupSide, Equals, Target)
	c.Assert(seeds[0].Status, Equals, Started)
	c.Assert(seeds[0].Stage, Equals, Prepare)
	c.Assert(seeds[0].Retries, Equals, 0)
}

func (s *AgentTestSuite) TestReadRecentSeeds(c *C) {
	targetTestAgent1 := s.testAgents["agent1"]
	sourceTestAgent1 := s.testAgents["agent2"]
	targetTestAgent2 := s.testAgents["agent3"]
	sourceTestAgent2 := s.testAgents["agent4"]

	s.registerAgents(c)

	targetAgent1, sourceAgent1 := s.getSeedAgents(c, targetTestAgent1, sourceTestAgent1)
	targetAgent2, sourceAgent2 := s.getSeedAgents(c, targetTestAgent2, sourceTestAgent2)

	seedID, err := NewSeed("Mydumper", targetAgent1, sourceAgent1)
	c.Assert(err, IsNil)
	c.Assert(seedID, Equals, int64(1))

	seedID, err = NewSeed("Mydumper", targetAgent2, sourceAgent2)
	c.Assert(err, IsNil)
	c.Assert(seedID, Equals, int64(2))

	seeds, err := ReadRecentSeeds()
	c.Assert(err, IsNil)
	c.Assert(seeds, HasLen, 2)

	for _, seed := range seeds {
		if seed.SeedID == 1 {
			c.Assert(seed.TargetHostname, Equals, targetTestAgent1.agent.Info.Hostname)
			c.Assert(seed.SourceHostname, Equals, sourceTestAgent1.agent.Info.Hostname)
		} else {
			c.Assert(seed.TargetHostname, Equals, targetTestAgent2.agent.Info.Hostname)
			c.Assert(seed.SourceHostname, Equals, sourceTestAgent2.agent.Info.Hostname)
		}
		c.Assert(seed.SeedMethod, Equals, Mydumper)
		c.Assert(seed.BackupSide, Equals, Target)
		c.Assert(seed.Status, Equals, Started)
		c.Assert(seed.Stage, Equals, Prepare)
		c.Assert(seed.Retries, Equals, 0)
	}
}

func (s *AgentTestSuite) TestReadRecentSeedsForAgentInStatus(c *C) {
	targetTestAgent := s.testAgents["agent1"]
	sourceTestAgent := s.testAgents["agent2"]

	s.registerAgents(c)

	targetAgent, sourceAgent := s.getSeedAgents(c, targetTestAgent, sourceTestAgent)

	seedID, err := NewSeed("Mydumper", targetAgent, sourceAgent)
	c.Assert(err, IsNil)
	c.Assert(seedID, Equals, int64(1))

	for _, agent := range []*Agent{targetAgent, sourceAgent} {
		seeds, err := ReadRecentSeedsForAgentInStatus(agent, Started, "limit 1")
		c.Assert(err, IsNil)
		c.Assert(seeds, HasLen, 1)
		c.Assert(seeds[0].TargetHostname, Equals, targetTestAgent.agent.Info.Hostname)
		c.Assert(seeds[0].SourceHostname, Equals, sourceTestAgent.agent.Info.Hostname)
		c.Assert(seeds[0].SeedMethod, Equals, Mydumper)
		c.Assert(seeds[0].BackupSide, Equals, Target)
		c.Assert(seeds[0].Status, Equals, Started)
		c.Assert(seeds[0].Stage, Equals, Prepare)
		c.Assert(seeds[0].Retries, Equals, 0)
	}

	for _, agent := range []*Agent{targetAgent, sourceAgent} {
		seeds, err := ReadRecentSeedsForAgentInStatus(agent, Running, "limit 1")
		c.Assert(err, IsNil)
		c.Assert(seeds, HasLen, 0)
	}
}

func (s *AgentTestSuite) TestReadActiveSeedsForAgent(c *C) {
	targetTestAgent := s.testAgents["agent1"]
	sourceTestAgent := s.testAgents["agent2"]

	s.registerAgents(c)

	targetAgent, sourceAgent := s.getSeedAgents(c, targetTestAgent, sourceTestAgent)

	seedID, err := NewSeed("Mydumper", targetAgent, sourceAgent)
	c.Assert(err, IsNil)
	c.Assert(seedID, Equals, int64(1))

	for _, agent := range []*Agent{targetAgent, sourceAgent} {
		seeds, err := ReadActiveSeedsForAgent(agent)
		c.Assert(err, IsNil)
		c.Assert(seeds, HasLen, 1)
		c.Assert(seeds[0].TargetHostname, Equals, targetTestAgent.agent.Info.Hostname)
		c.Assert(seeds[0].SourceHostname, Equals, sourceTestAgent.agent.Info.Hostname)
		c.Assert(seeds[0].SeedMethod, Equals, Mydumper)
		c.Assert(seeds[0].BackupSide, Equals, Target)
		c.Assert(seeds[0].Status, Equals, Started)
		c.Assert(seeds[0].Stage, Equals, Prepare)
		c.Assert(seeds[0].Retries, Equals, 0)
	}

	activeSeed := s.readSeed(c, seedID, targetAgent.Info.Hostname, sourceAgent.Info.Hostname, Mydumper, Target, Started, Prepare, 0)
	activeSeed.Status = Completed
	activeSeed.updateSeedData()

	for _, agent := range []*Agent{targetAgent, sourceAgent} {
		seeds, err := ReadActiveSeedsForAgent(agent)
		c.Assert(err, IsNil)
		c.Assert(seeds, HasLen, 0)
	}

}

func (s *AgentTestSuite) TestGetSeedAgents(c *C) {
	targetTestAgent := s.testAgents["agent1"]
	sourceTestAgent := s.testAgents["agent2"]

	s.registerAgents(c)

	targetAgent, sourceAgent := s.getSeedAgents(c, targetTestAgent, sourceTestAgent)

	seedID, err := NewSeed("Mydumper", targetAgent, sourceAgent)
	c.Assert(err, IsNil)
	c.Assert(seedID, Equals, int64(1))

	seed := s.readSeed(c, seedID, targetAgent.Info.Hostname, sourceAgent.Info.Hostname, Mydumper, Target, Started, Prepare, 0)

	seedTargetAgent, seedSourceAgent, err := seed.GetSeedAgents()
	c.Assert(err, IsNil)
	c.Assert(targetAgent.Info, DeepEquals, seedTargetAgent.Info)
	c.Assert(sourceAgent.Info, DeepEquals, seedSourceAgent.Info)
}

func (s *AgentTestSuite) TestProcessSeeds(c *C) {
	targetTestAgent := s.testAgents["agent1"]
	sourceTestAgent := s.testAgents["agent2"]

	s.registerAgents(c)

	targetAgent, sourceAgent := s.getSeedAgents(c, targetTestAgent, sourceTestAgent)

	seedID, err := NewSeed("Mydumper", targetAgent, sourceAgent)
	c.Assert(err, IsNil)
	c.Assert(seedID, Equals, int64(1))

	// Orchestrator registered seed. It's will have first stage - Prepare and status Started
	seed := s.readSeed(c, seedID, targetAgent.Info.Hostname, sourceAgent.Info.Hostname, Mydumper, Target, Started, Prepare, 0)

	// ProcessSeeds. Orchestrator will ask agents to start Prepare stage. So it's state would change from Started to Running
	ProcessSeeds()
	seed = s.readSeed(c, seedID, targetAgent.Info.Hostname, sourceAgent.Info.Hostname, Mydumper, Target, Running, Prepare, 0)

	// simulate that prepare stage is running on both agents
	for _, agent := range []*testAgent{targetTestAgent, sourceTestAgent} {
		agent.agentSeedStageStatus.SeedID = seedID
		agent.agentSeedStageStatus.Stage = Prepare
		agent.agentSeedStageStatus.Hostname = agent.agent.Info.Hostname
		agent.agentSeedStageStatus.Timestamp = time.Now()
		agent.agentSeedStageStatus.Status = Running
		agent.agentSeedStageStatus.Details = "processing prepare stage"
	}

	// check that SeedStageStates in Orchestator DB are the same, that were read from agents
	s.readSeedStageStates(c, seed, 2, targetTestAgent, sourceTestAgent)

	// ProcessSeeds. As seed is in status Running, Orchestrator will ask agent about seed stage status. As it's not Completed or Errored, it will just record SeedStageState in DB.
	ProcessSeeds()
	seed = s.readSeed(c, seedID, targetAgent.Info.Hostname, sourceAgent.Info.Hostname, Mydumper, Target, Running, Prepare, 0)
	s.readSeedStageStates(c, seed, 2, targetTestAgent, sourceTestAgent)

	// now simulate that target agent completed Prepare stage
	targetTestAgent.agentSeedStageStatus.Status = Completed
	targetTestAgent.agentSeedStageStatus.Timestamp = time.Now()
	targetTestAgent.agentSeedStageStatus.Details = "completed prepare stage"

	// ProcessSeeds. We have stage: Prepare and status: Running as only target agent completed Prepare stage, but in SeedStageStates we will have one Completed record and one Running
	ProcessSeeds()
	seed = s.readSeed(c, seedID, targetAgent.Info.Hostname, sourceAgent.Info.Hostname, Mydumper, Target, Running, Prepare, 0)
	s.readSeedStageStates(c, seed, 2, targetTestAgent, sourceTestAgent)

	// now simulate that source agent completed Prepare stage
	sourceTestAgent.agentSeedStageStatus.Status = Completed
	sourceTestAgent.agentSeedStageStatus.Timestamp = time.Now()
	sourceTestAgent.agentSeedStageStatus.Details = "completed prepare stage"

	// ProcessSeeds. Now both agents completed Prepare stage, so Orchestrator will move Seed to Backup stage with status Started. And we will have SeedStageState records with stage Prepare and status Completed
	ProcessSeeds()
	seed = s.readSeed(c, seedID, targetAgent.Info.Hostname, sourceAgent.Info.Hostname, Mydumper, Target, Started, Backup, 0)
	s.readSeedStageStates(c, seed, 1, targetTestAgent, sourceTestAgent)

	// ProcessSeeds. Orchestrator will ask agent to start Backup stage. So it's state would change from Started to Running
	ProcessSeeds()
	seed = s.readSeed(c, seedID, targetAgent.Info.Hostname, sourceAgent.Info.Hostname, Mydumper, Target, Running, Backup, 0)

	// simulate that Backup stage is running on target agent(as seedSide for Mydumper method is Target)
	targetTestAgent.agentSeedStageStatus.Stage = Backup
	targetTestAgent.agentSeedStageStatus.Timestamp = time.Now()
	targetTestAgent.agentSeedStageStatus.Status = Running
	targetTestAgent.agentSeedStageStatus.Details = "running backup stage"
	s.readSeedStageStates(c, seed, 1, targetTestAgent, sourceTestAgent)

	// ProcessSeeds. TargetAgent is still running Backup Stage, so seed state won't change and we will have one SeedStageState record from target agent
	ProcessSeeds()
	seed = s.readSeed(c, seedID, targetAgent.Info.Hostname, sourceAgent.Info.Hostname, Mydumper, Target, Running, Backup, 0)
	s.readSeedStageStates(c, seed, 1, targetTestAgent, sourceTestAgent)

	// now simulate that target agent completed Backup stage
	targetTestAgent.agentSeedStageStatus.Status = Completed
	targetTestAgent.agentSeedStageStatus.Details = "completed backup stage"

	// ProcessSeeds. Target agent had completed Backup stage, so Orchestrator will move Seed to Restore stage with status Started. And we will have one SeedStageState record with stage Backup and status Completed
	ProcessSeeds()
	seed = s.readSeed(c, seedID, targetAgent.Info.Hostname, sourceAgent.Info.Hostname, Mydumper, Target, Started, Restore, 0)
	s.readSeedStageStates(c, seed, 1, targetTestAgent, sourceTestAgent)

	// ProcessSeeds. Orchestrator will ask agent to start Restore stage. So it's state would change from Started to Running
	ProcessSeeds()
	seed = s.readSeed(c, seedID, targetAgent.Info.Hostname, sourceAgent.Info.Hostname, Mydumper, Target, Running, Restore, 0)

	// simulate that Backup stage is running on target agent(as restore is always processed by targetAgent)
	targetTestAgent.agentSeedStageStatus.Stage = Restore
	targetTestAgent.agentSeedStageStatus.Timestamp = time.Now()
	targetTestAgent.agentSeedStageStatus.Status = Running
	targetTestAgent.agentSeedStageStatus.Details = "running restore stage"
	s.readSeedStageStates(c, seed, 1, targetTestAgent, sourceTestAgent)

	// ProcessSeeds. TargetAgent is still running Restore Stage, so seed state won't change and we will have one SeedStageState record from target agent
	ProcessSeeds()
	seed = s.readSeed(c, seedID, targetAgent.Info.Hostname, sourceAgent.Info.Hostname, Mydumper, Target, Running, Restore, 0)
	s.readSeedStageStates(c, seed, 1, targetTestAgent, sourceTestAgent)

	// now simulate that target agent completed Restore stage
	targetTestAgent.agentSeedStageStatus.Status = Completed
	targetTestAgent.agentSeedStageStatus.Details = "completed restore stage"

	// ProcessSeeds. Target agent had completed Restore stage, so Orchestrator will move Seed to Cleanup stage with status Started. And we will have one SeedStageState record with stage Restore and status Completed
	ProcessSeeds()
	seed = s.readSeed(c, seedID, targetAgent.Info.Hostname, sourceAgent.Info.Hostname, Mydumper, Target, Started, Cleanup, 0)
	s.readSeedStageStates(c, seed, 1, targetTestAgent, sourceTestAgent)

	// ProcessSeeds. Orchestrator will ask agents to start Cleanup stage. So it's state would change from Started to Running
	ProcessSeeds()
	seed = s.readSeed(c, seedID, targetAgent.Info.Hostname, sourceAgent.Info.Hostname, Mydumper, Target, Running, Cleanup, 0)

	// simulate that cleanup stage is running on both agents
	for _, agent := range []*testAgent{targetTestAgent, sourceTestAgent} {
		agent.agentSeedStageStatus.Stage = Cleanup
		agent.agentSeedStageStatus.Timestamp = time.Now()
		agent.agentSeedStageStatus.Status = Running
		agent.agentSeedStageStatus.Details = "processing cleanup stage"
	}

	// check that SeedStageStates in Orchestator DB are the same, that were read from agents
	s.readSeedStageStates(c, seed, 2, targetTestAgent, sourceTestAgent)

	// ProcessSeeds. Both agents are still running Cleanup Stage, so seed state won't change and we will have 2 SeedStageState record from each agent
	ProcessSeeds()
	seed = s.readSeed(c, seedID, targetAgent.Info.Hostname, sourceAgent.Info.Hostname, Mydumper, Target, Running, Cleanup, 0)
	s.readSeedStageStates(c, seed, 2, targetTestAgent, sourceTestAgent)

	// now simulate that source agent completed Prepare stage
	sourceTestAgent.agentSeedStageStatus.Status = Completed
	sourceTestAgent.agentSeedStageStatus.Timestamp = time.Now()
	sourceTestAgent.agentSeedStageStatus.Details = "completed prepare stage"

	// ProcessSeeds. We have stage: Prepare and status: Running as only source agent completed Prepare stage, but in SeedStageStates we will have one Completed record and one Running
	ProcessSeeds()
	seed = s.readSeed(c, seedID, targetAgent.Info.Hostname, sourceAgent.Info.Hostname, Mydumper, Target, Running, Cleanup, 0)
	s.readSeedStageStates(c, seed, 2, targetTestAgent, sourceTestAgent)

	// now simulate that target agent completed Prepare stage
	targetTestAgent.agentSeedStageStatus.Status = Completed
	targetTestAgent.agentSeedStageStatus.Timestamp = time.Now()
	targetTestAgent.agentSeedStageStatus.Details = "completed prepare stage"

	// ProcessSeeds. Now both agents completed Cleanup stage, so Orchestrator will move Seed to ConnectSlave stage with status Started. And we will have 2 SeedStageState records with stage Cleanup and status Completed
	ProcessSeeds()
	seed = s.readSeed(c, seedID, targetAgent.Info.Hostname, sourceAgent.Info.Hostname, Mydumper, Target, Started, ConnectSlave, 0)
	s.readSeedStageStates(c, seed, 1, targetTestAgent, sourceTestAgent)

	// create containers with MySQL for ConnectSlave stage
	err = s.createTestAgentMySQLServer(sourceTestAgent, true, true)
	c.Assert(err, IsNil)
	err = s.createTestAgentMySQLServer(targetTestAgent, true, true)
	c.Assert(err, IsNil)
	err = s.createIPTablesRulesForReplication(targetTestAgent, sourceTestAgent)
	c.Assert(err, IsNil)

	//register agents one more time to update mysqlport
	s.registerAgents(c)

	// update targetAgent with source agent replication pos\gtid
	targetTestAgent.agentSeedMetadata = sourceTestAgent.agentSeedMetadata

	// update Orchestrator config
	config.Config.MySQLTopologyUser = "orc_topology"
	config.Config.MySQLTopologyPassword = "orc_topologypassword@"
	config.Config.ReplicationCredentialsQuery = "select username, password from orchestrator_meta.replication;"

	// ProcessSeeds. This is the last stage, so we will have seed in completed Status.
	ProcessSeeds()
	seed = s.readSeed(c, seedID, targetAgent.Info.Hostname, sourceAgent.Info.Hostname, Mydumper, Target, Completed, ConnectSlave, 0)
	// simulate that Backup stage is running on target agent(as restore is always processed by targetAgent)
	targetTestAgent.agentSeedStageStatus.Stage = ConnectSlave
	targetTestAgent.agentSeedStageStatus.Timestamp = time.Now()
	targetTestAgent.agentSeedStageStatus.Status = Completed
	targetTestAgent.agentSeedStageStatus.Details = "completed connectSlave stage"
	s.readSeedStageStates(c, seed, 1, targetTestAgent, sourceTestAgent)
}
