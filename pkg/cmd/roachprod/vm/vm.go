// Copyright 2018 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.

package vm

import (
	"fmt"
	"log"
	"regexp"
	"time"

	"github.com/cockroachdb/cockroach/pkg/cmd/roachprod/config"
	"github.com/pkg/errors"
	"github.com/spf13/pflag"
	"golang.org/x/sync/errgroup"
)

// A VM is an abstract representation of a specific machine instance.  This type is used across
// the various cloud providers supported by roachprod.
type VM struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	// If non-empty, indicates that some or all of the data in the VM instance
	// is not present or otherwise invalid.
	Errors   []error       `json:"errors"`
	Lifetime time.Duration `json:"lifetime"`
	// The provider-internal DNS name for the VM instance
	DNS string `json:"dns"`
	// The name of the cloud provider that hosts the VM instance
	Provider string `json:"provider"`
	// The provider-specific id for the instance.  This may or may not be the same as Name, depending
	// on whether or not the cloud provider automatically assigns VM identifiers.
	ProviderID string `json:"provider_id"`
	PrivateIP  string `json:"private_ip"`
	PublicIP   string `json:"public_ip"`
	// The username that should be used to connect to the VM.
	RemoteUser string `json:"remote_user"`
	// The VPC value defines an equivalency set for VMs that can route
	// to one another via private IP addresses.  We use this later on
	// when determining whether or not cluster member should advertise
	// their public or private IP.
	VPC         string `json:"vpc"`
	MachineType string `json:"machine_type"`
	Zone        string `json:"zone"`
}

// Name generates the name for the i'th node in a cluster.
func Name(cluster string, idx int) string {
	return fmt.Sprintf("%s-%0.4d", cluster, idx)
}

// Error values for VM.Error
var (
	ErrBadNetwork   = errors.New("could not determine network information")
	ErrInvalidName  = errors.New("invalid VM name")
	ErrNoExpiration = errors.New("could not determine expiration")
)

var regionRE = regexp.MustCompile(`(.*[^-])-?[a-z]$`)

// IsLocal returns true if the VM represents the local host.
func (vm *VM) IsLocal() bool {
	return vm.Zone == config.Local
}

// Locality returns the cloud, region, and zone for the VM.  We want to include the cloud, since
// GCE and AWS use similarly-named regions (e.g. us-east-1)
func (vm *VM) Locality() string {
	var region string
	if vm.IsLocal() {
		region = vm.Zone
	} else if match := regionRE.FindStringSubmatch(vm.Zone); len(match) == 2 {
		region = match[1]
	} else {
		log.Fatalf("unable to parse region from zone %q", vm.Zone)
	}
	return fmt.Sprintf("cloud=%s,region=%s,zone=%s", vm.Provider, region, vm.Zone)
}

// List TODO(peter): document
type List []VM

func (vl List) Len() int           { return len(vl) }
func (vl List) Swap(i, j int)      { vl[i], vl[j] = vl[j], vl[i] }
func (vl List) Less(i, j int) bool { return vl[i].Name < vl[j].Name }

// Names sxtracts all VM.Name entries from the List
func (vl List) Names() []string {
	ret := make([]string, len(vl))
	for i, vm := range vl {
		ret[i] = vm.Name
	}
	return ret
}

// ProviderIDs extracts all ProviderID values from the List.
func (vl List) ProviderIDs() []string {
	ret := make([]string, len(vl))
	for i, vm := range vl {
		ret[i] = vm.ProviderID
	}
	return ret
}

// CreateOpts is the set of options when creating VMs.
type CreateOpts struct {
	Lifetime       time.Duration
	GeoDistributed bool
	VMProviders    []string
	SSDOpts        struct {
		UseLocalSSD bool
		// NoExt4Barrier, if set, makes the "-o nobarrier" flag be used when
		// mounting the SSD. Ignored if UseLocalSSD is not set.
		NoExt4Barrier bool
	}
}

// ProviderFlags is a hook point for Providers to supply additional,
// provider-specific flags to various roachprod commands. In general, the flags
// should be prefixed with the provider's name to prevent collision between
// similar options.
//
// If a new command is added (perhaps `roachprod enlarge`) that needs
// additional provider- specific flags, add a similarly-named method
// `ConfigureEnlargeFlags` to mix in the additional flags.
type ProviderFlags interface {
	// Configures a FlagSet with any options relevant to the `create` command.
	ConfigureCreateFlags(*pflag.FlagSet)
	// Configures a FlagSet with any options relevant to cluster manipulation
	// commands (`create`, `destroy`, `list`, `sync` and `gc`).
	ConfigureClusterFlags(*pflag.FlagSet)
}

// A Provider is a source of virtual machines running on some hosting platform.
type Provider interface {
	CleanSSH() error
	ConfigSSH() error
	Create(names []string, opts CreateOpts) error
	Delete(vms List) error
	Extend(vms List, lifetime time.Duration) error
	// Return the account name associated with the provider
	FindActiveAccount() (string, error)
	// Returns a hook point for extending top-level roachprod tooling flags
	Flags() ProviderFlags
	List() (List, error)
	// The name of the Provider, which will also surface in the top-level Providers map.
	Name() string
}

// Providers contains all known Provider instances. This is initialized by subpackage init() functions.
var Providers = map[string]Provider{}

// AllProviderNames returns the names of all known vm Providers.  This is useful with the
// ProvidersSequential or ProvidersParallel methods.
func AllProviderNames() []string {
	var ret []string
	for name := range Providers {
		ret = append(ret, name)
	}
	return ret
}

// FanOut collates a collection of VMs by their provider and invoke the callbacks in parallel.
func FanOut(list List, action func(Provider, List) error) error {
	var m = map[string]List{}
	for _, vm := range list {
		m[vm.Provider] = append(m[vm.Provider], vm)
	}

	var g errgroup.Group
	for name, vms := range m {
		// capture loop variables
		n := name
		v := vms
		g.Go(func() error {
			p, ok := Providers[n]
			if !ok {
				return errors.Errorf("unknown provider name: %s", n)
			}
			return action(p, v)
		})
	}

	return g.Wait()
}

// Memoizes return value from FindActiveAccounts.
var cachedActiveAccounts map[string]string

// FindActiveAccounts queries the active providers for the name of the user
// account.
func FindActiveAccounts() (map[string]string, error) {
	source := cachedActiveAccounts

	if source == nil {
		// Ask each Provider for its active account name.
		source = map[string]string{}
		err := ProvidersSequential(AllProviderNames(), func(p Provider) error {
			account, err := p.FindActiveAccount()
			if err != nil {
				return err
			}
			if len(account) > 0 {
				source[p.Name()] = account
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		cachedActiveAccounts = source
	}

	// Return a copy.
	ret := make(map[string]string, len(source))
	for k, v := range source {
		ret[k] = v
	}

	return ret, nil
}

// ForProvider resolves the Provider with the given name and executes the
// action.
func ForProvider(named string, action func(Provider) error) error {
	p, ok := Providers[named]
	if !ok {
		return errors.Errorf("unknown vm provider: %s", named)
	}
	if err := action(p); err != nil {
		return errors.Wrapf(err, "in provider: %s", named)
	}
	return nil
}

// ProvidersParallel concurrently executes actions for each named Provider.
func ProvidersParallel(named []string, action func(Provider) error) error {
	var g errgroup.Group
	for _, name := range named {
		// capture loop variable
		n := name
		g.Go(func() error {
			return ForProvider(n, action)
		})
	}
	return g.Wait()
}

// ProvidersSequential sequentially executes actions for each named Provider.
func ProvidersSequential(named []string, action func(Provider) error) error {
	for _, name := range named {
		if err := ForProvider(name, action); err != nil {
			return err
		}
	}
	return nil
}
