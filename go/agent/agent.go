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
	"github.com/github/orchestrator/go/inst"
)

// LogicalVolume describes an LVM volume
type LogicalVolume struct {
	Name            string
	GroupName       string
	Path            string
	IsSnapshot      bool
	SnapshotPercent float64
}

// Mount describes a file system mount point
type Mount struct {
	Path       string
	Device     string
	LVPath     string
	FileSystem string
	IsMounted  bool
	DiskUsage  int64
}

// MySQLDatabase describes a MySQL database
type MySQLDatabase struct {
	Engines []Engine
	Size    int64
}

type AgentInfo struct {
	LocalSnapshotsHosts  []string         // AvailableLocalSnapshots in Orchestrator
	SnaphostHosts        []string         // AvailableSnapshots in Orchestrator
	LogicalVolumes       []*LogicalVolume // pass by reference ??
	MountPoint           *Mount           // pass by reference ??
	BackupDir            string
	BackupDirDiskFree    int64
	MySQLRunning         bool
	MySQLDatadir         string
	MySQLDatadirDiskUsed int64
	MySQLDatadirDiskFree int64
	MySQLVersion         string
	MySQLDatabases       map[string]*MySQLDatabase
	MySQLErrorLogTail    []string
}

type AgentParams struct {
	Hostname              string
	Port                  int
	Token                 string
	MySQLPort             int
	AvailiableSeedMethods map[SeedMethod]SeedMethodOpts
}

type Agent struct {
	Params        *AgentParams
	Info          *AgentInfo
	LastSubmitted string
}

// SeedOperationState represents a single state (step) in a seed operation
type SeedOperationState struct {
	SeedStateId    int64
	SeedId         int64
	StateTimestamp string
	Action         string
	ErrorMessage   string
}

// Build an instance key for a given agent
func (a *Agent) GetInstance() *inst.InstanceKey {
	return &inst.InstanceKey{Hostname: a.Params.Hostname, Port: a.Params.Port}
}
