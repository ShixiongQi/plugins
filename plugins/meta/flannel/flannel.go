// Copyright 2015 CNI authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// This is a "meta-plugin". It reads in its own netconf, combines it with
// the data from flannel generated subnet file and then invokes a plugin
// like bridge or ipvlan to do the real work.

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"log"

	"github.com/containernetworking/cni/pkg/invoke"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/version"

	bv "github.com/containernetworking/plugins/pkg/utils/buildversion"
)

const (
	defaultSubnetFile = "/run/flannel/subnet.env"
	defaultDataDir    = "/var/lib/cni/flannel"
)

type NetConf struct {
	types.NetConf

	SubnetFile    string                 `json:"subnetFile"`
	DataDir       string                 `json:"dataDir"`
	Delegate      map[string]interface{} `json:"delegate"`
	RuntimeConfig map[string]interface{} `json:"runtimeConfig,omitempty"`
}

type subnetEnv struct {
	nw     *net.IPNet
	sn     *net.IPNet
	mtu    *uint
	ipmasq *bool
}

func (se *subnetEnv) missing() string {
	m := []string{}

	if se.nw == nil {
		m = append(m, "FLANNEL_NETWORK")
	}
	if se.sn == nil {
		m = append(m, "FLANNEL_SUBNET")
	}
	if se.mtu == nil {
		m = append(m, "FLANNEL_MTU")
	}
	if se.ipmasq == nil {
		m = append(m, "FLANNEL_IPMASQ")
	}
	return strings.Join(m, ", ")
}

func loadFlannelNetConf(bytes []byte) (*NetConf, error) {
	n := &NetConf{
		SubnetFile: defaultSubnetFile,
		DataDir:    defaultDataDir,
	}
	if err := json.Unmarshal(bytes, n); err != nil {
		return nil, fmt.Errorf("failed to load netconf: %v", err)
	}

	return n, nil
}

func loadFlannelSubnetEnv(fn string) (*subnetEnv, error) {
	f, err := os.Open(fn)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	se := &subnetEnv{}

	s := bufio.NewScanner(f)
	for s.Scan() {
		parts := strings.SplitN(s.Text(), "=", 2)
		switch parts[0] {
		case "FLANNEL_NETWORK":
			_, se.nw, err = net.ParseCIDR(parts[1])
			if err != nil {
				return nil, err
			}

		case "FLANNEL_SUBNET":
			_, se.sn, err = net.ParseCIDR(parts[1])
			if err != nil {
				return nil, err
			}

		case "FLANNEL_MTU":
			mtu, err := strconv.ParseUint(parts[1], 10, 32)
			if err != nil {
				return nil, err
			}
			se.mtu = new(uint)
			*se.mtu = uint(mtu)

		case "FLANNEL_IPMASQ":
			ipmasq := parts[1] == "true"
			se.ipmasq = &ipmasq
		}
	}
	if err := s.Err(); err != nil {
		return nil, err
	}

	if m := se.missing(); m != "" {
		return nil, fmt.Errorf("%v is missing %v", fn, m)
	}

	return se, nil
}

func saveScratchNetConf(containerID, dataDir string, netconf []byte) error {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return err
	}
	path := filepath.Join(dataDir, containerID)
	return ioutil.WriteFile(path, netconf, 0600)
}

func consumeScratchNetConf(containerID, dataDir string) ([]byte, error) {
	path := filepath.Join(dataDir, containerID)
	// Ignore errors when removing - Per spec safe to continue during DEL
	defer os.Remove(path)

	return ioutil.ReadFile(path)
}

func delegateAdd(cid, dataDir string, netconf map[string]interface{}) error {
	netconfBytes, err := json.Marshal(netconf)
	if err != nil {
		return fmt.Errorf("error serializing delegate netconf: %v", err)
	}

	// save the rendered netconf for cmdDel
	if err = saveScratchNetConf(cid, dataDir, netconfBytes); err != nil {
		return err
	}

	result, err := invoke.DelegateAdd(context.TODO(), netconf["type"].(string), netconfBytes, nil)
	if err != nil {
		return err
	}

	return result.Print()
}

func hasKey(m map[string]interface{}, k string) bool {
	_, ok := m[k]
	return ok
}

func isString(i interface{}) bool {
	_, ok := i.(string)
	return ok
}

func cmdAdd(args *skel.CmdArgs) error {

	logFileName := "/users/sqi009/flannel-start-time.log"
	logFile, _  := os.OpenFile(logFileName,os.O_RDWR|os.O_APPEND|os.O_CREATE,0644)
	defer logFile.Close()
	debugLog := log.New(logFile,"[Info: flannel.go]",log.Lmicroseconds)
	debugLog.Println("[flannel] cmdAdd start")
	debugLog.Println("[flannel] loadFlannelNetConf start")
	n, err := loadFlannelNetConf(args.StdinData)
	if err != nil {
		return err
	}
	debugLog.Println("[flannel] loadFlannelSubnetEnv start")
	fenv, err := loadFlannelSubnetEnv(n.SubnetFile)
	if err != nil {
		return err
	}
	debugLog.Println("[flannel] loadFlannelSubnetEnv fin")
	debugLog.Println("[flannel] parse delegate plugin start")
	if n.Delegate == nil {
		n.Delegate = make(map[string]interface{})
	} else {
		if hasKey(n.Delegate, "type") && !isString(n.Delegate["type"]) {
			return fmt.Errorf("'delegate' dictionary, if present, must have (string) 'type' field")
		}
		if hasKey(n.Delegate, "name") {
			return fmt.Errorf("'delegate' dictionary must not have 'name' field, it'll be set by flannel")
		}
		if hasKey(n.Delegate, "ipam") {
			return fmt.Errorf("'delegate' dictionary must not have 'ipam' field, it'll be set by flannel")
		}
	}

	if n.RuntimeConfig != nil {
		n.Delegate["runtimeConfig"] = n.RuntimeConfig
	}
	debugLog.Println("[flannel] cmdAdd finish")
	debugLog.Println("[flannel] doCmdAdd start")
	return doCmdAdd(args, n, fenv)
}

func cmdDel(args *skel.CmdArgs) error {

	logFileName := "/users/sqi009/flannel_cmdDel_info.log"
	// logFile, _  := os.Create(logFileName)
	logFile, _  := os.OpenFile(logFileName,os.O_RDWR|os.O_APPEND|os.O_CREATE,0644)
	defer logFile.Close()
	debugLog := log.New(logFile,"[Info: flannel.go]",log.Lmicroseconds)
	debugLog.Println("[flannel] cmdDel start")

	nc, err := loadFlannelNetConf(args.StdinData)
	if err != nil {
		return err
	}

	if nc.RuntimeConfig != nil {
		if nc.Delegate == nil {
			nc.Delegate = make(map[string]interface{})
		}
		nc.Delegate["runtimeConfig"] = nc.RuntimeConfig
	}
	debugLog.Println("[flannel] cmdDel finish")
	return doCmdDel(args, nc)
}

func main() {
	skel.PluginMain(cmdAdd, cmdCheck, cmdDel, version.All, bv.BuildString("flannel"))
}

func cmdCheck(args *skel.CmdArgs) error {
	// TODO: implement
	return nil
}
