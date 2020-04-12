/*
Copyright 2019 The HAProxy Ingress Controller Authors.

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

package haproxy

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"

	"github.com/jcmoraisjr/haproxy-ingress/pkg/haproxy/template"
	hatypes "github.com/jcmoraisjr/haproxy-ingress/pkg/haproxy/types"
)

// Config ...
type Config interface {
	AcquireTCPBackend(servicename string, port int) *hatypes.TCPBackend
	AcquireBackend(namespace, name, port string) *hatypes.Backend
	FindBackend(namespace, name, port string) *hatypes.Backend
	ConfigDefaultBackend(defaultBackend *hatypes.Backend)
	ConfigDefaultX509Cert(filename string)
	AddUserlist(name string, users []hatypes.User) *hatypes.Userlist
	FindUserlist(name string) *hatypes.Userlist
	Frontend() *hatypes.Frontend
	FrontendGroup() *hatypes.FrontendGroup
	SyncConfig()
	BuildFrontendGroup() error
	BuildBackendMaps() error
	DefaultBackend() *hatypes.Backend
	AcmeData() *hatypes.AcmeData
	Acme() *hatypes.Acme
	Global() *hatypes.Global
	TCPBackends() []*hatypes.TCPBackend
	Hosts() *hatypes.Hosts
	Backends() []*hatypes.Backend
	Userlists() []*hatypes.Userlist
	Equals(other Config) bool
}

type config struct {
	// external state, non haproxy data, cannot reflect in Config.Equals()
	acmeData *hatypes.AcmeData
	// haproxy internal state
	acme            *hatypes.Acme
	fgroup          *hatypes.FrontendGroup
	mapsTemplate    *template.Config
	mapsDir         string
	global          *hatypes.Global
	frontend        *hatypes.Frontend
	hosts           *hatypes.Hosts
	tcpbackends     []*hatypes.TCPBackend
	backends        []*hatypes.Backend
	userlists       []*hatypes.Userlist
	defaultBackend  *hatypes.Backend
	defaultX509Cert string
}

type options struct {
	mapsTemplate *template.Config
	mapsDir      string
}

func createConfig(options options) *config {
	mapsTemplate := options.mapsTemplate
	if mapsTemplate == nil {
		mapsTemplate = template.CreateConfig()
	}
	return &config{
		acmeData:     &hatypes.AcmeData{},
		acme:         &hatypes.Acme{},
		global:       &hatypes.Global{},
		frontend:     &hatypes.Frontend{Name: "_front001"},
		hosts:        &hatypes.Hosts{},
		mapsTemplate: mapsTemplate,
		mapsDir:      options.mapsDir,
	}
}

func (c *config) AcquireTCPBackend(servicename string, port int) *hatypes.TCPBackend {
	for _, backend := range c.tcpbackends {
		if backend.Name == servicename && backend.Port == port {
			return backend
		}
	}
	backend := &hatypes.TCPBackend{
		Name: servicename,
		Port: port,
	}
	c.tcpbackends = append(c.tcpbackends, backend)
	sort.Slice(c.tcpbackends, func(i, j int) bool {
		back1 := c.tcpbackends[i]
		back2 := c.tcpbackends[j]
		if back1.Name == back2.Name {
			return back1.Port < back2.Port
		}
		return back1.Name < back2.Name
	})
	return backend
}

func (c *config) sortBackends() {
	sort.Slice(c.backends, func(i, j int) bool {
		if c.backends[i] == c.defaultBackend {
			return false
		}
		if c.backends[j] == c.defaultBackend {
			return true
		}
		return c.backends[i].ID < c.backends[j].ID
	})
}

func (c *config) AcquireBackend(namespace, name, port string) *hatypes.Backend {
	if backend := c.FindBackend(namespace, name, port); backend != nil {
		return backend
	}
	backend := createBackend(namespace, name, port)
	c.backends = append(c.backends, backend)
	c.sortBackends()
	return backend
}

func (c *config) FindBackend(namespace, name, port string) *hatypes.Backend {
	for _, b := range c.backends {
		if b.Namespace == namespace && b.Name == name && b.Port == port {
			return b
		}
	}
	return nil
}

func createBackend(namespace, name, port string) *hatypes.Backend {
	return &hatypes.Backend{
		ID:        buildID(namespace, name, port),
		Namespace: namespace,
		Name:      name,
		Port:      port,
		Server:    hatypes.ServerConfig{InitialWeight: 1},
		Endpoints: []*hatypes.Endpoint{},
	}
}

func buildID(namespace, name, port string) string {
	return fmt.Sprintf("%s_%s_%s", namespace, name, port)
}

func (c *config) ConfigDefaultBackend(defaultBackend *hatypes.Backend) {
	if c.defaultBackend != nil {
		def := c.defaultBackend
		def.ID = buildID(def.Namespace, def.Name, def.Port)
	}
	c.defaultBackend = defaultBackend
	if c.defaultBackend != nil {
		c.defaultBackend.ID = "_default_backend"
	}
	c.sortBackends()
}

func (c *config) ConfigDefaultX509Cert(filename string) {
	c.defaultX509Cert = filename
}

func (c *config) AddUserlist(name string, users []hatypes.User) *hatypes.Userlist {
	userlist := &hatypes.Userlist{
		Name:  name,
		Users: users,
	}
	sort.Slice(users, func(i, j int) bool {
		return users[i].Name < users[j].Name
	})
	c.userlists = append(c.userlists, userlist)
	sort.Slice(c.userlists, func(i, j int) bool {
		return c.userlists[i].Name < c.userlists[j].Name
	})
	return userlist
}

func (c *config) FindUserlist(name string) *hatypes.Userlist {
	return nil
}

func (c *config) Frontend() *hatypes.Frontend {
	return c.frontend
}

func (c *config) FrontendGroup() *hatypes.FrontendGroup {
	return c.fgroup
}

// SyncConfig does final synchronization, just before write
// maps and config files to disk. These tasks should be done
// during ingress, services and endpoint parsing, but most of
// them need to start after all objects are parsed.
func (c *config) SyncConfig() {
	if c.hosts.HasSSLPassthrough() {
		// using ssl-passthrough config, so need a `mode tcp`
		// frontend with `inspect-delay` and `req.ssl_sni`
		bindName := fmt.Sprintf("%s_socket", c.frontend.Name)
		c.frontend.BindName = bindName
		c.frontend.BindSocket = fmt.Sprintf("unix@/var/run/%s.sock", bindName)
		c.frontend.AcceptProxy = true
	} else {
		// One single HAProxy's frontend and bind
		c.frontend.BindName = "_public"
		c.frontend.BindSocket = c.global.Bind.HTTPSBind
		c.frontend.AcceptProxy = c.global.Bind.AcceptProxy
	}
	for _, host := range c.hosts.Items {
		if host.SSLPassthrough {
			// no action if ssl-passthrough
			continue
		}
		if host.HasTLSAuth() {
			for _, path := range host.Paths {
				backend := c.FindBackend(path.Backend.Namespace, path.Backend.Name, path.Backend.Port)
				if backend != nil {
					backend.TLS.HasTLSAuth = true
				}
			}
		}
		if c.global.StrictHost && host.FindPath("/") == nil {
			var back *hatypes.Backend
			defaultHost := c.hosts.DefaultHost()
			if defaultHost != nil {
				if path := defaultHost.FindPath("/"); path != nil {
					hback := path.Backend
					back = c.FindBackend(hback.Namespace, hback.Name, hback.Port)
				}
			}
			if back == nil {
				// TODO c.defaultBackend can be nil; create a valid
				// _error404 backend, remove `if nil` from host.AddPath()
				// and from `for range host.Paths` on map building.
				back = c.defaultBackend
			}
			host.AddPath(back, "/")
		}
	}
}

func (c *config) BuildFrontendGroup() error {
	// tested thanks to instance_test templating tests
	// ideas to make a nice test or a nice refactor are welcome
	maps := hatypes.CreateMaps()
	fgroup := &hatypes.FrontendGroup{
		HTTPFrontsMap:     maps.AddMap(c.mapsDir + "/_global_http_front.map"),
		HTTPRootRedirMap:  maps.AddMap(c.mapsDir + "/_global_http_root_redir.map"),
		HTTPSRedirMap:     maps.AddMap(c.mapsDir + "/_global_https_redir.map"),
		SSLPassthroughMap: maps.AddMap(c.mapsDir + "/_global_sslpassthrough.map"),
		VarNamespaceMap:   maps.AddMap(c.mapsDir + "/_global_k8s_ns.map"),
		//
		HostBackendsMap:            maps.AddMap(c.mapsDir + "/_front001_host.map"),
		RootRedirMap:               maps.AddMap(c.mapsDir + "/_front001_root_redir.map"),
		MaxBodySizeMap:             maps.AddMap(c.mapsDir + "/_front001_max_body_size.map"),
		SNIBackendsMap:             maps.AddMap(c.mapsDir + "/_front001_sni.map"),
		TLSInvalidCrtErrorList:     maps.AddMap(c.mapsDir + "/_front001_inv_crt.list"),
		TLSInvalidCrtErrorPagesMap: maps.AddMap(c.mapsDir + "/_front001_inv_crt_redir.map"),
		TLSNoCrtErrorList:          maps.AddMap(c.mapsDir + "/_front001_no_crt.list"),
		TLSNoCrtErrorPagesMap:      maps.AddMap(c.mapsDir + "/_front001_no_crt_redir.map"),
		//
		CrtList:       maps.AddMap(c.mapsDir + "/_front001_bind_crt.list"),
		UseServerList: maps.AddMap(c.mapsDir + "/_front001_use_server.list"),
	}
	fgroup.CrtList.AppendItem(c.defaultX509Cert)
	// Some maps use yes/no answers instead of a list with found/missing keys
	// This approach avoid overlap:
	//  1. match with path_beg/map_beg, /path has a feature and a declared /path/sub doesn't have
	//  2. *.host.domain wildcard/alias/alias-regex has a feature and a declared sub.host.domain doesn't have
	yesno := map[bool]string{true: "yes", false: "no"}
	for _, host := range c.hosts.Items {
		if host.SSLPassthrough {
			rootPath := host.FindPath("/")
			if rootPath == nil {
				return fmt.Errorf("missing root path on host %s", host.Hostname)
			}
			fgroup.SSLPassthroughMap.AppendHostname(host.Hostname, rootPath.Backend.ID)
			fgroup.HTTPSRedirMap.AppendHostname(host.Hostname+"/", yesno[host.HTTPPassthroughBackend == ""])
			if host.HTTPPassthroughBackend != "" {
				fgroup.HTTPFrontsMap.AppendHostname(host.Hostname+"/", host.HTTPPassthroughBackend)
			}
			// ssl-passthrough is as simple as that, jump to the next host
			continue
		}
		//
		// Starting here to the end of this for loop has only HTTP/L7 map configuration
		//
		// TODO implement deny 413 and move all MaxBodySize stuff to backend
		maxBodySizes := map[string]int64{}
		for _, path := range host.Paths {
			backend := c.FindBackend(path.Backend.Namespace, path.Backend.Name, path.Backend.Port)
			base := host.Hostname + path.Path
			hasSSLRedirect := false
			if host.TLS.HasTLS() && backend != nil {
				hasSSLRedirect = backend.HasSSLRedirectHostpath(base)
			}
			// TODO use only root path if all uri has the same conf
			fgroup.HTTPSRedirMap.AppendHostname(host.Hostname+path.Path, yesno[hasSSLRedirect])
			var aliasName, aliasRegex string
			// TODO warn in logs about ignoring alias name due to hostname colision
			if host.Alias.AliasName != "" && c.hosts.FindHost(host.Alias.AliasName) == nil {
				aliasName = host.Alias.AliasName + path.Path
			}
			if host.Alias.AliasRegex != "" {
				aliasRegex = host.Alias.AliasRegex + path.Path
			}
			backendID := path.Backend.ID
			if host.HasTLSAuth() {
				fgroup.SNIBackendsMap.AppendHostname(base, backendID)
				fgroup.SNIBackendsMap.AppendAliasName(aliasName, backendID)
				fgroup.SNIBackendsMap.AppendAliasRegex(aliasRegex, backendID)
			} else {
				fgroup.HostBackendsMap.AppendHostname(base, backendID)
				fgroup.HostBackendsMap.AppendAliasName(aliasName, backendID)
				fgroup.HostBackendsMap.AppendAliasRegex(aliasRegex, backendID)
			}
			if backend != nil {
				if maxBodySize := backend.MaxBodySizeHostpath(base); maxBodySize > 0 {
					maxBodySizes[base] = maxBodySize
				}
			}
			if !hasSSLRedirect || c.global.Bind.HasFrontingProxy() {
				fgroup.HTTPFrontsMap.AppendHostname(base, backendID)
			}
			var ns string
			if host.VarNamespace {
				ns = path.Backend.Namespace
			} else {
				ns = "-"
			}
			fgroup.VarNamespaceMap.AppendHostname(base, ns)
		}
		// TODO implement deny 413 and move all MaxBodySize stuff to backend
		if len(maxBodySizes) > 0 {
			// add all paths of the same host to avoid overlap
			// 0 (zero) means unlimited
			for _, path := range host.Paths {
				base := host.Hostname + path.Path
				fgroup.MaxBodySizeMap.AppendHostname(base, strconv.FormatInt(maxBodySizes[base], 10))
			}
		}
		if host.HasTLSAuth() {
			fgroup.TLSInvalidCrtErrorList.AppendHostname(host.Hostname, "")
			if !host.TLS.CAVerifyOptional {
				fgroup.TLSNoCrtErrorList.AppendHostname(host.Hostname, "")
			}
			page := host.TLS.CAErrorPage
			if page != "" {
				fgroup.TLSInvalidCrtErrorPagesMap.AppendHostname(host.Hostname, page)
				if !host.TLS.CAVerifyOptional {
					fgroup.TLSNoCrtErrorPagesMap.AppendHostname(host.Hostname, page)
				}
			}
		}
		// TODO wildcard/alias/alias-regex hostname can overlap
		// a configured domain which doesn't have rootRedirect
		if host.RootRedirect != "" {
			fgroup.HTTPRootRedirMap.AppendHostname(host.Hostname, host.RootRedirect)
			fgroup.RootRedirMap.AppendHostname(host.Hostname, host.RootRedirect)
		}
		fgroup.UseServerList.AppendHostname(host.Hostname, "")
		//
		tls := host.TLS
		crtFile := tls.TLSFilename
		if crtFile == "" {
			crtFile = c.defaultX509Cert
		}
		if crtFile != c.defaultX509Cert || tls.CAFilename != "" {
			// has custom cert or tls auth
			//
			// TODO optimization: distinct hostnames that shares crt, ca and crl
			// can be combined into a single line. Note that this is usually the exception.
			// TODO this NEED its own template file.
			var crtListConfig string
			if tls.CAFilename == "" {
				crtListConfig = fmt.Sprintf("%s %s", crtFile, host.Hostname)
			} else {
				var crl string
				if tls.CRLFilename != "" {
					crl = " crl-file " + tls.CRLFilename
				}
				crtListConfig = fmt.Sprintf("%s [ca-file %s%s verify optional] %s", crtFile, tls.CAFilename, crl, host.Hostname)
			}
			fgroup.CrtList.AppendItem(crtListConfig)
		}
	}
	if err := writeMaps(maps, c.mapsTemplate); err != nil {
		return err
	}
	c.fgroup = fgroup
	return nil
}

func (c *config) BuildBackendMaps() error {
	// TODO rename HostMap types to HAProxyMap
	maps := hatypes.CreateMaps()
	for _, backend := range c.backends {
		mapsPrefix := c.mapsDir + "/_back_" + backend.ID
		if backend.NeedACL() {
			pathsMap := maps.AddMap(mapsPrefix + "_idpath.map")
			for _, path := range backend.Paths {
				pathsMap.AppendPath(path.Hostpath, path.ID)
			}
			backend.PathsMap = pathsMap
		}
	}
	return writeMaps(maps, c.mapsTemplate)
}

func writeMaps(maps *hatypes.HostsMaps, template *template.Config) error {
	for _, hmap := range maps.Items {
		if err := template.WriteOutput(hmap.Match, hmap.MatchFile); err != nil {
			return err
		}
		if len(hmap.Regex) > 0 {
			if err := template.WriteOutput(hmap.Regex, hmap.RegexFile); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *config) DefaultBackend() *hatypes.Backend {
	return c.defaultBackend
}

func (c *config) AcmeData() *hatypes.AcmeData {
	return c.acmeData
}

func (c *config) Acme() *hatypes.Acme {
	return c.acme
}

func (c *config) Global() *hatypes.Global {
	return c.global
}

func (c *config) TCPBackends() []*hatypes.TCPBackend {
	return c.tcpbackends
}

func (c *config) Hosts() *hatypes.Hosts {
	return c.hosts
}

func (c *config) Backends() []*hatypes.Backend {
	return c.backends
}

func (c *config) Userlists() []*hatypes.Userlist {
	return c.userlists
}

func (c *config) Equals(other Config) bool {
	c2, ok := other.(*config)
	if !ok {
		return false
	}
	// (config struct): external state, cannot reflect in Config.Equals()
	copy := *c2
	copy.acmeData = c.acmeData
	return reflect.DeepEqual(c, &copy)
}
