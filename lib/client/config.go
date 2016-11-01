// Copyright (c) 2016 Tigera, Inc. All rights reserved.

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

package client

import (
	"encoding/json"
	"fmt"
	"strconv"

	log "github.com/Sirupsen/logrus"
	"github.com/projectcalico/libcalico-go/lib/backend/model"
	"github.com/projectcalico/libcalico-go/lib/errors"
	"github.com/projectcalico/libcalico-go/lib/net"
)

type ConfigLocation int8

const (
	ConfigLocationNone ConfigLocation = iota
	ConfigLocationNode
	ConfigLocationGlobal
)

var logToBgp = map[string]string{
	"none":     "none",
	"debug":    "debug",
	"info":     "info",
	"warning":  "none",
	"error":    "none",
	"critical": "none",
}

const (
	GlobalDefaultASNumber       = 64511
	GlobalDefaultLogLevel       = "info"
	GlobalDefaultIPIP           = false
	GlobalDefaultNodeToNodeMesh = true
)

// ConfigInterface provides methods for setting, unsetting and retrieving low
// level config options.
type ConfigInterface interface {
	SetNodeToNodeMesh(bool) error
	GetNodeToNodeMesh() (bool, error)
	SetGlobalASNumber(uint64) error
	GetGlobalASNumber() (uint64, error)
	SetGlobalIPIP(bool) error
	GetGlobalIPIP() (bool, error)
	SetNodeIPIPTunnelAddress(string, *net.IP) error
	GetNodeIPIPTunnelAddress(string) (*net.IP, error)
	SetGlobalLogLevel(string) error
	GetGlobalLogLevel() (string, error)
	SetNodeLogLevel(string, string) error
	SetNodeLogLevelUseGlobal(string) error
	GetNodeLogLevel(string) (string, ConfigLocation, error)
}

// config implements ConfigInterface
type config struct {
	c *Client
}

// newConfig returns a new ConfigInterface bound to the supplied client.
func newConfigs(c *Client) ConfigInterface {
	return &config{c}
}

// The configuration interface provides the ability to set and get low-level,
// or system-wide configuration options.

// SetNodeToNodeMesh sets the enabled state of the system-wide node-to-node mesh.
// When this is enabled, each calico/node instance automatically establishes a
// full BGP peering mesh between all nodes that support BGP.
func (c *config) SetNodeToNodeMesh(enabled bool) error {
	b, _ := json.Marshal(nodeToNodeMesh{Enabled: enabled})
	_, err := c.c.backend.Apply(&model.KVPair{
		Key:   model.GlobalBGPConfigKey{Name: "node_mesh"},
		Value: string(b),
	})
	return err
}

// GetNodeToNodeMesh returns the current enabled state of the system-wide
// node-to-node mesh option.  See SetNodeToNodeMesh for details.
func (c *config) GetNodeToNodeMesh() (bool, error) {
	var n nodeToNodeMesh
	if s, err := c.getValue(model.GlobalBGPConfigKey{Name: "node_mesh"}); err != nil {
		log.Info("Error getting node mesh")
		return false, err
	} else if s == nil {
		log.Info("Return default node to node mesh")
		return GlobalDefaultNodeToNodeMesh, nil
	} else if err = json.Unmarshal([]byte(*s), &n); err != nil {
		log.Info("Error parsing node to node mesh")
		return false, err
	} else {
		log.Info("Returning configured node to node mesh")
		return n.Enabled, nil
	}
}

// SetGlobalASNumber sets the global AS Number used by the BGP agent running
// on each node.  This may be overridden by an explicitly configured value in
// the node resource.
func (c *config) SetGlobalASNumber(asNumber uint64) error {
	_, err := c.c.backend.Apply(&model.KVPair{
		Key:   model.GlobalBGPConfigKey{Name: "as_num"},
		Value: strconv.FormatUint(asNumber, 10),
	})
	return err
}

// SetGlobalASNumber gets the global AS Number used by the BGP agent running
// on each node.  See SetGlobalASNumber for more details.
func (c *config) GetGlobalASNumber() (uint64, error) {
	if s, err := c.getValue(model.GlobalBGPConfigKey{Name: "as_num"}); err != nil {
		return 0, err
	} else if s == nil {
		return GlobalDefaultASNumber, nil
	} else if asn, err := strconv.ParseUint(*s, 10, 64); err != nil {
		return 0, err
	} else {
		return asn, nil
	}
}

// SetGlobalIPIP sets the global IP in IP enabled setting inherited by all nodes
// in the Calico cluster.  When IP in IP is enabled, packets routed to IP addresses
// that fall within an IP in IP enabled Calico IP Pool, will be routed over an
// IP in IP tunnel.
func (c *config) SetGlobalIPIP(enabled bool) error {
	_, err := c.c.backend.Apply(&model.KVPair{
		Key:   model.GlobalConfigKey{Name: "IpInIpEnabled"},
		Value: strconv.FormatBool(enabled),
	})
	return err
}

// GetGlobalIPIP gets the global IPIP enabled setting.  See SetGlobalIPIP for details.
func (c *config) GetGlobalIPIP() (bool, error) {
	if s, err := c.getValue(model.GlobalConfigKey{Name: "IpInIpEnabled"}); err != nil {
		return false, err
	} else if s == nil {
		return GlobalDefaultIPIP, nil
	} else if enabled, err := strconv.ParseBool(*s); err != nil {
		return false, err
	} else {
		return enabled, nil
	}
}

// SetNodeIPIPTunnelAddress sets the IP in IP tunnel address for a specific node.
// Felix will use this to configure the tunnel.
func (c *config) SetNodeIPIPTunnelAddress(node string, ip *net.IP) error {
	key := model.HostConfigKey{Hostname: node, Name: "IpInIpTunnelAddr"}
	if ip == nil {
		err := c.deleteConfig(key)
		return err
	} else {
		_, err := c.c.backend.Apply(&model.KVPair{
			Key:   key,
			Value: ip.String(),
		})
		return err
	}
}

// GetNodeIPIPTunnelAddress gets the IP in IP tunnel address for a specific node.
// See SetNodeIPIPTunnelAddress for more details.
func (c *config) GetNodeIPIPTunnelAddress(node string) (*net.IP, error) {
	ip := &net.IP{}
	if s, err := c.getValue(model.HostConfigKey{Hostname: node, Name: "IpInIpTunnelAddr"}); err != nil {
		return nil, err
	} else if s == nil {
		return nil, nil
	} else if err = ip.UnmarshalText([]byte(*s)); err != nil {
		return nil, err
	} else {
		return ip, nil
	}
}

// SetGlobalLogLevel sets the system global log level used by the node.  This
// may be overridden on a per-node basis.
func (c *config) SetGlobalLogLevel(level string) error {
	return c.setLogLevel(
		level,
		model.GlobalConfigKey{Name: "LogLevelScreen"},
		model.GlobalConfigKey{Name: "logLevel"})
}

// GetGlobalLogLevel gets the current system global log level.
func (c *config) GetGlobalLogLevel() (string, error) {
	s, err := c.getValue(model.GlobalConfigKey{Name: "LogLevelScreen"})
	if err != nil {
		return "", err
	} else if s == nil {
		return GlobalDefaultLogLevel, nil
	} else {
		return *s, nil
	}
}

// SetNodeLogLevel sets the node specific log level.  This overrides the global
// log level.
func (c *config) SetNodeLogLevel(node string, level string) error {
	return c.setLogLevel(level,
		model.HostConfigKey{Hostname: node, Name: "LogLevelScreen"},
		model.HostBGPConfigKey{Hostname: node, Name: "logLevel"})
}

// SetNodeLogLevelUseGlobal sets the node to use the global log level.
func (c *config) SetNodeLogLevelUseGlobal(node string) error {
	kf := model.HostConfigKey{Hostname: node, Name: "LogLevelScreen"}
	kb := model.HostBGPConfigKey{Hostname: node, Name: "logLevel"}
	err1 := c.deleteConfig(kf)
	err2 := c.deleteConfig(kb)

	// Return error or nil.
	if err1 != nil {
		return err1
	}
	return err2
}

// GetNodeLogLevel returns the current effective log level for the node.  The
// second return parameter indicates whether the value is explicitly set on the
// node or inherited from the system-wide global value.
func (c *config) GetNodeLogLevel(node string) (string, ConfigLocation, error) {
	s, err := c.getValue(model.HostConfigKey{Hostname: node, Name: "LogLevelScreen"})
	if err != nil {
		return "", ConfigLocationNone, err
	} else if s == nil {
		l, err := c.GetGlobalLogLevel()
		return l, ConfigLocationGlobal, err
	} else {
		return *s, ConfigLocationNode, nil
	}
}

// setLogLevel sets the log level fields with the appropriate log string value.
func (c *config) setLogLevel(level string, felixKey, bgpKey model.Key) error {
	bgpLevel, ok := logToBgp[level]
	if !ok {
		return erroredField("loglevel", level)
	}
	_, err1 := c.c.backend.Apply(&model.KVPair{
		Key:   felixKey,
		Value: level,
	})
	_, err2 := c.c.backend.Apply(&model.KVPair{
		Key:   bgpKey,
		Value: bgpLevel,
	})

	// Return error or nil.
	if err1 != nil {
		return err1
	}
	return err2
}

// deleteConfig deletes a resource and ignores deleted errors.
func (c *config) deleteConfig(key model.Key) error {
	err := c.c.backend.Delete(&model.KVPair{Key: key})
	if err != nil {
		if _, ok := err.(errors.ErrorResourceDoesNotExist); !ok {
			return err
		}
	}
	return nil
}

// getValue returns the string value (pointer) or nil if the key does not
// exist in the datastore.
func (c *config) getValue(key model.Key) (*string, error) {
	kv, err := c.c.backend.Get(key)
	if err != nil {
		if _, ok := err.(errors.ErrorResourceDoesNotExist); ok {
			return nil, nil
		} else {
			return nil, err
		}
	} else {
		value := kv.Value.(string)
		return &value, nil
	}
}

// erroredField creates an ErrorValidation.
func erroredField(name string, value interface{}) error {
	err := errors.ErrorValidation{
		ErroredFields: []errors.ErroredField{
			errors.ErroredField{
				Name:  name,
				Value: fmt.Sprint(value),
			},
		},
	}
	return err
}

// nodeToNodeMesh is a struct containing whether node-to-node mesh is enabled.  It can be
// JSON marshalled into the correct structure that is understood by the Calico BGP component.
type nodeToNodeMesh struct {
	Enabled bool `json:"enabled"`
}
