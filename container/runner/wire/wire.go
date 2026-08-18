// Package wire is the runner's composition root: it assembles the concrete adapters into a ready
// app.Service. Deliberately the ONE place that imports every adapter, so the domain, ports and app
// layers stay adapter-agnostic. Swapping an adapter is a one-line change here.
package wire

import (
	"github.com/crispuscrew/zinc/container/runner/adapters/dbusproxy"
	"github.com/crispuscrew/zinc/container/runner/adapters/fs"
	"github.com/crispuscrew/zinc/container/runner/adapters/host"
	"github.com/crispuscrew/zinc/container/runner/adapters/netenforce"
	"github.com/crispuscrew/zinc/container/runner/adapters/podman"
	"github.com/crispuscrew/zinc/container/runner/adapters/waylandctx"
	"github.com/crispuscrew/zinc/container/runner/app"
	"github.com/crispuscrew/zinc/container/runner/ports"
)

// Service wires a given store with the podman adapters, the egress enforcer and the D-Bus broker.
// The broker is built from the host environment here rather than per call, because Stop is given a
// config and no HostOptions and still has to tear down the proxy and its socket directory.
func Service(store ports.Store) app.Service {
	opt := host.Options()
	return app.New(store, podman.Runtime{}, podman.Builder{}, podman.Resolver{}, netenforce.Enforcer{},
		dbusproxy.New(opt.NetfilterImage, opt), waylandctx.Broker{})
}

// DefaultService builds the production service against the standard on-disk store
// (~/.config/zinc/apps).
func DefaultService() (app.Service, error) {
	store, err := fs.Default()
	if err != nil {
		return app.Service{}, err
	}
	return Service(store), nil
}
