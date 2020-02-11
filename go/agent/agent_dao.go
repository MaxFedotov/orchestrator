/*
   Copyright 2014 Outbrain Inc.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package agent

import (
	"encoding/json"
	"fmt"

	"github.com/github/orchestrator/go/config"
	"github.com/github/orchestrator/go/db"
	"github.com/openark/golib/log"
	"github.com/openark/golib/sqlutils"
)

// readAgentsInfo reads agents information from backend table
func readAgentsInfo(whereCondition string, args []interface{}, limit string) ([]*Agent, error) {
	res := []*Agent{}
	query := fmt.Sprintf(`
		select
			hostname,
			port,
			token,
			last_seen,
			mysql_port
		from
			host_agent
		%s
		order by
			hostname desc
		%s
		`, whereCondition, limit)
	err := db.QueryOrchestrator(query, args, func(m sqlutils.RowMap) error {
		agent := &Agent{Info: &Info{}, Data: &Data{}}
		agent.Info.Hostname = m.GetString("hostname")
		agent.Info.Port = m.GetInt("port")
		agent.Info.MySQLPort = m.GetInt("mysql_port")
		agent.Info.Token = m.GetString("token")
		agent.LastSeen = m.GetTime("last_seen")
		// add to cache
		res = append(res, agent)
		return nil
	})

	if err != nil {
		log.Errore(err)
	}
	return res, err
}

// readAgentsInfo reads agent information with agent data from backend table
func readAgents(whereCondition string, args []interface{}, limit string) ([]*Agent, error) {
	res := []*Agent{}
	query := fmt.Sprintf(`
		SELECT
			ha.hostname,
			ha.port,
			ha.token,
			ha.last_seen,
			ha.mysql_port,
			ha.status,
			ha.data,
			di.suggested_cluster_alias
		FROM
			host_agent ha
		LEFT JOIN database_instance di ON di.hostname = ha.hostname AND di.port = ha.port
		%s
		ORDER BY
			di.suggested_cluster_alias ASC, ha.hostname ASC
		%s
		`, whereCondition, limit)
	err := db.QueryOrchestrator(query, args, func(m sqlutils.RowMap) error {
		agent := &Agent{Info: &Info{}, Data: &Data{}}
		agent.Info.Hostname = m.GetString("hostname")
		agent.Info.Port = m.GetInt("port")
		agent.Info.MySQLPort = m.GetInt("mysql_port")
		agent.Info.Token = m.GetString("token")
		agent.LastSeen = m.GetTime("last_seen")
		agent.ClusterName = m.GetString("suggested_cluster_alias")
		err := json.Unmarshal([]byte(m.GetString("data")), agent.Data)
		if err != nil {
			return log.Errore(err)
		}
		res = append(res, agent)
		return nil
	})

	if err != nil {
		log.Errore(err)
	}
	return res, err
}

// registerAgent inserts info about agent to database
func (agent *Agent) registerAgent() error {
	agentData, err := json.Marshal(agent.Data)
	if err != nil {
		return log.Errore(err)
	}
	_, err = db.ExecOrchestrator(`
			replace
				into host_agent (
					hostname, port, token, last_seen, mysql_port, status, data
				) VALUES (
					?, ?, ?, NOW(), ?, ?, ?
				)
			`,
		agent.Info.Hostname,
		agent.Info.Port,
		agent.Info.Token,
		agent.Info.MySQLPort,
		Active.String(),
		agentData,
	)
	return err
}

// updateAgentLastChecked updates the last_check timestamp in the orchestrator backed database
// for a given agent
func (agent *Agent) updateAgentLastChecked() error {
	_, err := db.ExecOrchestrator(`
        	update
        		host_agent
        	set
        		last_checked = NOW()
			where
				hostname = ?`,
		agent.Info.Hostname,
	)
	return err
}

func (agent *Agent) updateAgentStatus(status AgentStatus) error {
	_, err := db.ExecOrchestrator(`
        	update
        		host_agent
        	set
        		status = ?
			where
				hostname = ?`,
		status.String(),
		agent.Info.Hostname,
	)
	return err
}

// updateAgentData updates the data in the orchestrator backend database
// for a given agent
func (agent *Agent) updateAgentData() error {
	agentData, err := json.Marshal(agent.Data)
	if err != nil {
		return log.Errore(err)
	}
	_, err = db.ExecOrchestrator(`
        	update
        		host_agent
        	set
				data = ?,
				status = ?,
				last_seen = NOW()
			where
				hostname = ?`,
		agentData,
		Active.String(),
		agent.Info.Hostname,
	)
	return err
}

// ForgetLongUnseenAgents will remove entries of all agents that have long since been last seen.
func ForgetLongUnseenAgents() error {
	_, err := db.ExecOrchestrator(`
			delete
				from host_agent
			where
				last_seen < NOW() - interval ? hour`,
		config.Config.UnseenAgentForgetHours,
	)
	return err
}
