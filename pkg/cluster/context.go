/*
Copyright 2018 The Kubernetes Authors.

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

package cluster

import (
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"

	"sigs.k8s.io/kind/pkg/cluster/config"
	"sigs.k8s.io/kind/pkg/cluster/config/encoding"
	"sigs.k8s.io/kind/pkg/cluster/internal/create"
	"sigs.k8s.io/kind/pkg/cluster/internal/meta"
	"sigs.k8s.io/kind/pkg/cluster/logs"
	"sigs.k8s.io/kind/pkg/cluster/nodes"
	logutil "sigs.k8s.io/kind/pkg/log"
)

// Context is used to create / manipulate kubernetes-in-docker clusters
// See: NewContext()
type Context struct {
	*meta.ClusterMeta
}

// similar to valid docker container names, but since we will prefix
// and suffix this name, we can relax it a little
// see NewContext() for usage
// https://godoc.org/github.com/docker/docker/daemon/names#pkg-constants
var validNameRE = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)

// DefaultName is the default Context name
// TODO(bentheelder): consider removing automatic prefixing in favor
// of letting the user specify the full name..
const DefaultName = "kind"

// NewContext returns a new cluster management context
// if name is "" the default name will be used
func NewContext(name string) *Context {
	if name == "" {
		name = DefaultName
	}
	return &Context{
		ClusterMeta: meta.NewClusterMeta(name),
	}
}

// Validate will be called before creating new resources using the context
// It will not be called before deleting or listing resources, so as to allow
// contexts based around previously valid values to be used in newer versions
// You can call this early yourself to check validation before creation calls,
// though it will be called internally.
func (c *Context) Validate() error {
	// validate the name
	if !validNameRE.MatchString(c.Name()) {
		return errors.Errorf(
			"'%s' is not a valid cluster name, cluster names must match `%s`",
			c.Name(), validNameRE.String(),
		)
	}
	return nil
}

// ControlPlaneMeta tracks various outputs that are relevant to the control plane created with Kind.
// Here we can define things like ports and listen or bind addresses as needed.
type ControlPlaneMeta struct {
	// APIServerPort is the port that the container is forwarding to the
	// Kubernetes API server running in the container
	APIServerPort int
}

// GetControlPlaneMeta attempts to retrieve / compute metadata about
// the control plane for the context's cluster
// NOTE: due to refactoring this is currently non-functional (!)
// TODO(bentheelder): fix this
func (c *Context) GetControlPlaneMeta() (*ControlPlaneMeta, error) {
	return nil, errors.New("needs-reimplementation")
}

// ClusterName returns the Kubernetes cluster name based on the context name
func (c *Context) ClusterName() string {
	return c.Name()
}

//CreateOption is a Context configuration option supplied to create
type CreateOption func(*create.Context)

// Retain configures create to retain nodes after failing for debugging pourposes
func Retain(retain bool) CreateOption {
	return func(c *create.Context) {
		c.Retain = retain
	}
}

// WaitForReady configures create to use interval as maximum wait time for the control plane node to be ready
func WaitForReady(interval time.Duration) CreateOption {
	return func(c *create.Context) {
		// NB. WaitForReady option does not apply directly to create, but it should be forwarded to the exec command
		c.ExecOptions = append(c.ExecOptions, create.WaitForReady(interval))
	}
}

// Create provisions and starts a kubernetes-in-docker cluster
func (c *Context) Create(cfg *config.Config, options ...CreateOption) error {
	// default config fields (important for usage as a library, where the config
	// may be constructed in memory rather than from disk)
	encoding.Scheme.Default(cfg)

	// then validate
	if err := cfg.Validate(); err != nil {
		return err
	}

	// derive info necessary for creation
	derived, err := create.Derive(cfg)
	if err != nil {
		return err
	}

	// validate node configuration
	if err := derived.Validate(); err != nil {
		return err
	}

	// init the create context and logging
	cc := &create.Context{
		Config:        cfg,
		DerivedConfig: derived,
		ClusterMeta:   c.ClusterMeta,
	}

	cc.Status = logutil.NewStatus(os.Stdout)
	cc.Status.MaybeWrapLogrus(log.StandardLogger())

	defer cc.Status.End(false)

	// apply create options
	for _, option := range options {
		option(cc)
	}

	// attempt to explicitly pull the required node images if they doesn't exist locally
	// we don't care if this errors, we'll still try to run which also pulls
	cc.EnsureNodeImages()

	// Create node containers implementing defined config Nodes
	nodeList, err := cc.ProvisionNodes()
	if err != nil {
		// In case of errors nodes are deleted (except if retain is explicitly set)
		log.Error(err)
		if !cc.Retain {
			c.Delete()
		}
		return err
	}
	cc.Status.End(true)

	// After creating node containers the Kubernetes provisioning is executed
	// By default `kind` executes all the actions required to get a fully working
	// Kubernetes cluster; please note that the list of actions automatically
	// adapt to the topology defined in config
	// TODO(fabrizio pandini): make the list of executed actions configurable from CLI
	err = cc.Exec(nodeList, []string{"haproxy", "config", "init", "join"}, cc.ExecOptions...)
	if err != nil {
		// In case of errors nodes are deleted (except if retain is explicitly set)
		log.Error(err)
		if !cc.Retain {
			c.Delete()
		}
		return err
	}

	// TODO: consider shell detection.
	if runtime.GOOS == "windows" {
		fmt.Printf(
			"Cluster creation complete. To setup KUBECONFIG:\n\n"+

				"For the default cmd.exe console call:\n"+
				"kind get kubeconfig-path > kindpath\n"+
				"set /p KUBECONFIG=<kindpath && del kindpath\n\n"+

				"for PowerShell call:\n"+
				"$env:KUBECONFIG=\"$(kind get kubeconfig-path --name=%[1]q)\"\n\n"+

				"For bash on Windows:\n"+
				"export KUBECONFIG=\"$(kind get kubeconfig-path --name=%[1]q)\"\n\n"+

				"You can now use the cluster:\n"+
				"kubectl cluster-info\n",
			cc.Name(),
		)
	} else {
		fmt.Printf(
			"Cluster creation complete. You can now use the cluster with:\n\n"+

				"export KUBECONFIG=\"$(kind get kubeconfig-path --name=%q)\"\n"+
				"kubectl cluster-info\n",
			cc.Name(),
		)
	}
	return nil
}

// Delete tears down a kubernetes-in-docker cluster
func (c *Context) Delete() error {
	n, err := c.ListNodes()
	if err != nil {
		return errors.Wrap(err, "error listing nodes")
	}

	// try to remove the kind kube config file generated by "kind create cluster"
	err = os.Remove(c.KubeConfigPath())
	if err != nil && !os.IsNotExist(err) {
		log.Warningf("Tried to remove %s but received error: %s\n", c.KubeConfigPath(), err)
	}

	// check if $KUBECONFIG is set and let the user know to unset if so
	if strings.Contains(os.Getenv("KUBECONFIG"), c.KubeConfigPath()) {
		fmt.Printf("$KUBECONFIG is still set to use %s even though that file has been deleted, remember to unset it\n", c.KubeConfigPath())
	}

	return nodes.Delete(n...)
}

// ListNodes returns the list of container IDs for the "nodes" in the cluster
func (c *Context) ListNodes() ([]nodes.Node, error) {
	return nodes.List("label=" + c.ClusterLabel())
}

// CollectLogs will populate dir with cluster logs and other debug files
func (c *Context) CollectLogs(dir string) error {
	nodes, err := c.ListNodes()
	if err != nil {
		return err
	}
	return logs.Collect(nodes, dir)
}
