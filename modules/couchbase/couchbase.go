package couchbase

import (
	"context"
	"errors"
	"fmt"
	"github.com/cenkalti/backoff/v4"
	"github.com/docker/go-connections/nat"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/tidwall/gjson"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	MGMT_PORT     = "8091"
	MGMT_SSL_PORT = "18091"

	VIEW_PORT     = "8092"
	VIEW_SSL_PORT = "18092"

	QUERY_PORT     = "8093"
	QUERY_SSL_PORT = "18093"

	SEARCH_PORT     = "8094"
	SEARCH_SSL_PORT = "18094"

	ANALYTICS_PORT     = "8095"
	ANALYTICS_SSL_PORT = "18095"

	EVENTING_PORT     = "8096"
	EVENTING_SSL_PORT = "18096"

	KV_PORT     = "11210"
	KV_SSL_PORT = "11207"
)

type clusterInit func(context.Context) error

// CouchbaseContainer represents the Couchbase container type used in the module
type CouchbaseContainer struct {
	testcontainers.Container
	config *Config
}

// StartContainer creates an instance of the Couchbase container type
func StartContainer(ctx context.Context, opts ...Option) (*CouchbaseContainer, error) {
	config := &Config{
		enabledServices: []service{kv, query, search, index},
		username:        "Administrator",
		password:        "password",
		imageName:       "couchbase:6.5.1",
	}

	for _, opt := range opts {
		opt(config)
	}

	req := testcontainers.ContainerRequest{
		Image:        config.imageName,
		ExposedPorts: exposePorts(config.enabledServices),
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return nil, err
	}

	couchbaseContainer := CouchbaseContainer{container, config}

	clusterInitFunc := []clusterInit{
		couchbaseContainer.waitUntilNodeIsOnline,
		couchbaseContainer.initializeIsEnterprise,
		couchbaseContainer.renameNode,
		couchbaseContainer.initializeServices,
		couchbaseContainer.setMemoryQuotas,
		couchbaseContainer.configureAdminUser,
		couchbaseContainer.configureExternalPorts,
	}

	if contains(config.enabledServices, index) {
		clusterInitFunc = append(clusterInitFunc, couchbaseContainer.configureIndexer)
	}

	clusterInitFunc = append(clusterInitFunc, couchbaseContainer.waitUntilAllNodesAreHealthy)

	for _, fn := range clusterInitFunc {
		if err = fn(ctx); err != nil {
			return nil, err
		}
	}

	err = couchbaseContainer.createBuckets(ctx)
	if err != nil {
		return nil, err
	}

	return &couchbaseContainer, nil
}

func exposePorts(enabledServices []service) []string {
	exposedPorts := []string{MGMT_PORT + "/tcp", MGMT_SSL_PORT + "/tcp"}

	for _, service := range enabledServices {
		for _, port := range service.ports {
			exposedPorts = append(exposedPorts, port+"/tcp")
		}
	}

	return exposedPorts
}

func (c *CouchbaseContainer) waitUntilAllNodesAreHealthy(ctx context.Context) error {
	var waitStrategy []wait.Strategy

	waitStrategy = append(waitStrategy, wait.ForHTTP("/pools/default").
		WithPort(MGMT_PORT).
		WithBasicCredentials(c.config.username, c.config.password).
		WithStatusCodeMatcher(func(status int) bool {
			return status == http.StatusOK
		}).
		WithResponseMatcher(func(body io.Reader) bool {
			response, err := io.ReadAll(body)
			if err != nil {
				return false
			}
			status := gjson.Get(string(response), "nodes.0.status")
			if status.String() != "healthy" {
				return false
			}

			return true
		}))

	if contains(c.config.enabledServices, query) {
		waitStrategy = append(waitStrategy, wait.ForHTTP("/admin/ping").
			WithPort(QUERY_PORT).
			WithBasicCredentials(c.config.username, c.config.password).
			WithStatusCodeMatcher(func(status int) bool {
				return status == http.StatusOK
			}),
		)
	}

	if contains(c.config.enabledServices, analytics) {
		waitStrategy = append(waitStrategy, wait.ForHTTP("/admin/ping").
			WithPort(ANALYTICS_PORT).
			WithBasicCredentials(c.config.username, c.config.password).
			WithStatusCodeMatcher(func(status int) bool {
				return status == http.StatusOK
			}))
	}

	if contains(c.config.enabledServices, eventing) {
		waitStrategy = append(waitStrategy, wait.ForHTTP("/api/v1/config").
			WithPort(EVENTING_PORT).
			WithBasicCredentials(c.config.username, c.config.password).
			WithStatusCodeMatcher(func(status int) bool {
				return status == http.StatusOK
			}))
	}

	return wait.ForAll(waitStrategy...).WaitUntilReady(ctx, c)
}

func (c *CouchbaseContainer) waitUntilNodeIsOnline(ctx context.Context) error {
	return wait.ForHTTP("/pools").
		WithPort(MGMT_PORT).
		WithStatusCodeMatcher(func(status int) bool {
			return status == http.StatusOK
		}).
		WaitUntilReady(ctx, c)
}

func (c *CouchbaseContainer) initializeIsEnterprise(ctx context.Context) error {
	response, err := c.doHttpRequest(ctx, MGMT_PORT, "/pools", http.MethodGet, nil, false)
	if err != nil {
		return err
	}

	c.config.isEnterprise = gjson.Get(string(response), "isEnterprise").Bool()

	if !c.config.isEnterprise {
		if contains(c.config.enabledServices, analytics) {
			return errors.New("the Analytics Service is only supported with the Enterprise version")
		}
		if contains(c.config.enabledServices, eventing) {
			return errors.New("the Eventing Service is only supported with the Enterprise version")
		}
	}

	return nil
}

func (c *CouchbaseContainer) renameNode(ctx context.Context) error {
	hostname, err := c.getInternalIPAddress(ctx)
	if err != nil {
		return err
	}

	body := map[string]string{
		"hostname": hostname,
	}

	_, err = c.doHttpRequest(ctx, MGMT_PORT, "/node/controller/rename", http.MethodPost, body, false)

	return err
}

func (c *CouchbaseContainer) initializeServices(ctx context.Context) error {
	body := map[string]string{
		"services": c.getEnabledServices(),
	}
	_, err := c.doHttpRequest(ctx, MGMT_PORT, "/node/controller/setupServices", http.MethodPost, body, false)

	return err
}

func (c *CouchbaseContainer) setMemoryQuotas(ctx context.Context) error {
	body := map[string]string{}

	for _, s := range c.config.enabledServices {
		if !s.hasQuota() {
			continue
		}

		quota := strconv.Itoa(s.minimumQuotaMb)
		if s.identifier == kv.identifier {
			body["memoryQuota"] = quota
		} else {
			body[s.identifier+"MemoryQuota"] = quota
		}
	}

	_, err := c.doHttpRequest(ctx, MGMT_PORT, "/pools/default", http.MethodPost, body, false)

	return err
}

func (c *CouchbaseContainer) configureAdminUser(ctx context.Context) error {
	body := map[string]string{
		"username": c.config.username,
		"password": c.config.password,
		"port":     "SAME",
	}

	_, err := c.doHttpRequest(ctx, MGMT_PORT, "/settings/web", http.MethodPost, body, false)

	return err
}

func (c *CouchbaseContainer) configureExternalPorts(ctx context.Context) error {
	host, _ := c.Host(ctx)
	mgmt, _ := c.MappedPort(ctx, MGMT_PORT)
	mgmtSSL, _ := c.MappedPort(ctx, MGMT_SSL_PORT)
	body := map[string]string{
		"hostname": host,
		"mgmt":     string(mgmt),
		"mgmtSSL":  string(mgmtSSL),
	}

	if contains(c.config.enabledServices, kv) {
		kv, _ := c.MappedPort(ctx, KV_PORT)
		kvSSL, _ := c.MappedPort(ctx, KV_SSL_PORT)
		capi, _ := c.MappedPort(ctx, VIEW_PORT)
		capiSSL, _ := c.MappedPort(ctx, VIEW_SSL_PORT)

		body["kv"] = string(kv)
		body["kvSSL"] = string(kvSSL)
		body["capi"] = string(capi)
		body["capiSSL"] = string(capiSSL)
	}

	if contains(c.config.enabledServices, query) {
		n1ql, _ := c.MappedPort(ctx, QUERY_PORT)
		n1qlSSL, _ := c.MappedPort(ctx, QUERY_SSL_PORT)

		body["n1ql"] = string(n1ql)
		body["n1qlSSL"] = string(n1qlSSL)
	}

	if contains(c.config.enabledServices, search) {
		fts, _ := c.MappedPort(ctx, SEARCH_PORT)
		ftsSSL, _ := c.MappedPort(ctx, SEARCH_SSL_PORT)

		body["fts"] = string(fts)
		body["ftsSSL"] = string(ftsSSL)
	}

	if contains(c.config.enabledServices, analytics) {
		cbas, _ := c.MappedPort(ctx, ANALYTICS_PORT)
		cbasSSL, _ := c.MappedPort(ctx, ANALYTICS_SSL_PORT)

		body["cbas"] = string(cbas)
		body["cbasSSL"] = string(cbasSSL)
	}

	if contains(c.config.enabledServices, eventing) {
		eventingAdminPort, _ := c.MappedPort(ctx, EVENTING_PORT)
		eventingSSL, _ := c.MappedPort(ctx, EVENTING_SSL_PORT)

		body["eventingAdminPort"] = string(eventingAdminPort)
		body["eventingSSL"] = string(eventingSSL)
	}

	_, err := c.doHttpRequest(ctx, MGMT_PORT, "/node/controller/setupAlternateAddresses/external", http.MethodPut, body, true)

	return err
}

func (c *CouchbaseContainer) configureIndexer(ctx context.Context) error {
	storageMode := "forestdb"
	if c.config.isEnterprise {
		storageMode = "memory_optimized"
	}

	body := map[string]string{
		"storageMode": storageMode,
	}

	_, err := c.doHttpRequest(ctx, MGMT_PORT, "/settings/indexes", http.MethodPost, body, true)

	return err
}

func (c *CouchbaseContainer) createBuckets(ctx context.Context) error {
	for _, bucket := range c.config.buckets {
		err := c.createBucket(ctx, bucket)
		if err != nil {
			return err
		}

		err = c.waitForAllServicesEnabled(ctx, bucket)
		if err != nil {
			return err
		}

		if contains(c.config.enabledServices, query) {
			err = c.isQueryKeyspacePresent(ctx, bucket)
			if err != nil {
				return err
			}
		}

		if bucket.queryPrimaryIndex {
			if !contains(c.config.enabledServices, query) {
				return fmt.Errorf("primary index creation for bucket %s ignored, since QUERY service is not present", bucket.name)
			}

			err = c.createPrimaryIndex(ctx, bucket)
			if err != nil {
				return err
			}

			err = c.isPrimaryIndexOnline(ctx, bucket, err)
			if err != nil {
				return err
			}

		}
	}

	return nil
}

func (c *CouchbaseContainer) isPrimaryIndexOnline(ctx context.Context, bucket bucket, err error) error {
	body := map[string]string{
		"statement": "SELECT count(*) > 0 AS online FROM system:indexes where keyspace_id = \"" +
			bucket.name +
			"\" and is_primary = true and state = \"online\"",
	}

	err = backoff.Retry(func() error {
		response, err := c.doHttpRequest(ctx, QUERY_PORT, "/query/service", http.MethodPost, body, true)
		if err != nil {
			return err
		}

		online := gjson.Get(string(response), "results.0.online").Bool()
		if !online {
			return errors.New("primary index state is not online")
		}

		return nil
	}, backoff.WithContext(backoff.NewExponentialBackOff(), ctx))

	return err
}

func (c *CouchbaseContainer) createPrimaryIndex(ctx context.Context, bucket bucket) error {
	body := map[string]string{
		"statement": "CREATE PRIMARY INDEX on `" + bucket.name + "`",
	}

	_, err := c.doHttpRequest(ctx, QUERY_PORT, "/query/service", http.MethodPost, body, true)

	return err
}

func (c *CouchbaseContainer) isQueryKeyspacePresent(ctx context.Context, bucket bucket) error {
	body := map[string]string{
		"statement": "SELECT COUNT(*) > 0 as present FROM system:keyspaces WHERE name = \"" + bucket.name + "\"",
	}

	err := backoff.Retry(func() error {
		response, err := c.doHttpRequest(ctx, QUERY_PORT, "/query/service", http.MethodPost, body, true)
		if err != nil {
			return err
		}
		present := gjson.Get(string(response), "results.0.present").Bool()
		if !present {
			return errors.New("query namespace is not present")
		}

		return nil
	}, backoff.WithContext(backoff.NewExponentialBackOff(), ctx))

	return err
}

func (c *CouchbaseContainer) waitForAllServicesEnabled(ctx context.Context, bucket bucket) error {
	err := wait.ForHTTP("/pools/default/b/"+bucket.name).
		WithPort(MGMT_PORT).
		WithBasicCredentials(c.config.username, c.config.password).
		WithStatusCodeMatcher(func(status int) bool {
			return status == http.StatusOK
		}).
		WithResponseMatcher(func(body io.Reader) bool {
			response, err := io.ReadAll(body)
			if err != nil {
				return false
			}
			return c.checkAllServicesEnabled(response)
		}).
		WaitUntilReady(ctx, c)

	return err
}

func (c *CouchbaseContainer) createBucket(ctx context.Context, bucket bucket) error {
	flushEnabled := "0"
	if bucket.flushEnabled {
		flushEnabled = "1"
	}
	body := map[string]string{
		"name":          bucket.name,
		"ramQuotaMB":    strconv.Itoa(bucket.quota),
		"flushEnabled":  flushEnabled,
		"replicaNumber": strconv.Itoa(bucket.numReplicas),
	}

	_, err := c.doHttpRequest(ctx, MGMT_PORT, "/pools/default/buckets", http.MethodPost, body, true)

	return err
}

func (c *CouchbaseContainer) doHttpRequest(ctx context.Context, port, path, method string, body map[string]string, auth bool) ([]byte, error) {
	form := url.Values{}
	for k, v := range body {
		form.Set(k, v)
	}

	url, err := c.getUrl(ctx, port, path)
	if err != nil {
		return nil, err
	}

	request, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}

	request.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	if auth {
		request.SetBasicAuth(c.config.username, c.config.password)
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	return io.ReadAll(response.Body)
}

func (c *CouchbaseContainer) getUrl(ctx context.Context, port, path string) (string, error) {
	host, err := c.Host(ctx)
	if err != nil {
		return "", err
	}

	mappedPort, err := c.MappedPort(ctx, nat.Port(port))
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("http://%s:%d%s", host, mappedPort.Int(), path), nil
}

func (c *CouchbaseContainer) getInternalIPAddress(ctx context.Context) (string, error) {
	networks, err := c.ContainerIP(ctx)
	if err != nil {
		return "", err
	}

	return networks, nil
}

func (c *CouchbaseContainer) getEnabledServices() string {
	identifiers := make([]string, len(c.config.enabledServices))
	for i, v := range c.config.enabledServices {
		identifiers[i] = v.identifier
	}

	return strings.Join(identifiers, ",")
}

func contains(services []service, service service) bool {
	for _, s := range services {
		if s.identifier == service.identifier {
			return true
		}
	}
	return false
}

func (c *CouchbaseContainer) checkAllServicesEnabled(rawConfig []byte) bool {
	nodeExt := gjson.Get(string(rawConfig), "nodesExt")
	if !nodeExt.Exists() {
		return false
	}

	for _, node := range nodeExt.Array() {
		services := node.Map()["services"]
		if !services.Exists() {
			return false
		}

		for _, s := range c.config.enabledServices {
			found := false
			for serviceName := range services.Map() {
				if strings.HasPrefix(serviceName, s.identifier) {
					found = true
				}
			}

			if !found {
				return false
			}
		}
	}

	return true
}
