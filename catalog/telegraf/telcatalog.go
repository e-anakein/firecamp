package telcatalog

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/cloudstax/firecamp/catalog"
	"github.com/cloudstax/firecamp/common"
	"github.com/cloudstax/firecamp/dns"
	"github.com/cloudstax/firecamp/manage"
	"github.com/golang/glog"
)

const (
	defaultVersion = "1.5"
	// ContainerImage is the main running container.
	ContainerImage = common.ContainerNamePrefix + "telegraf:" + defaultVersion

	// DefaultCollectIntervalSecs defines the default metrics collect interval, unit: seconds
	DefaultCollectIntervalSecs = 60

	ENV_MONITOR_COLLECT_INTERVAL = "COLLECT_INTERVAL"
	ENV_MONITOR_SERVICE_NAME     = "MONITOR_SERVICE_NAME"
	ENV_MONITOR_SERVICE_TYPE     = "MONITOR_SERVICE_TYPE"
	ENV_MONITOR_SERVICE_MEMBERS  = "MONITOR_SERVICE_MEMBERS"
	ENV_MONITOR_METRICS          = "MONITOR_METRICS"

	// Limits the max custom metrics size to 16KB
	maxMetricsLength = 16 * 1024

	ENV_REDIS_AUTH = "REDIS_AUTH"
)

// The InfluxData Telegraf catalog service, https://github.com/influxdata/telegraf

// ValidateRequest checks if the request is valid
func ValidateRequest(req *manage.CatalogCreateTelegrafRequest) error {
	if req.Options.CollectIntervalSecs <= 0 {
		return errors.New("Please specify the valid collect interval")
	}
	if len(req.Options.MonitorMetrics) > maxMetricsLength {
		return fmt.Errorf("Max custom metrics length should be within %d bytes", maxMetricsLength)
	}

	return nil
}

// GenDefaultCreateServiceRequest returns the default service creation request.
func GenDefaultCreateServiceRequest(platform string, region string, cluster string, service string,
	attr *common.ServiceAttr, monitorServiceMembers []*common.ServiceMember,
	opts *manage.CatalogTelegrafOptions, res *common.Resources) (*manage.CreateServiceRequest, error) {

	serviceEnvs, err := genServiceEnvs(attr)
	if err != nil {
		return nil, err
	}

	members := ""
	for i, m := range monitorServiceMembers {
		dnsname := dns.GenDNSName(m.MemberName, attr.DomainName)
		if i == 0 {
			members = dnsname
		} else {
			members += fmt.Sprintf(",%s", dnsname)
		}
	}

	envkvs := []*common.EnvKeyValuePair{
		&common.EnvKeyValuePair{Name: common.ENV_REGION, Value: region},
		&common.EnvKeyValuePair{Name: common.ENV_CLUSTER, Value: cluster},
		&common.EnvKeyValuePair{Name: common.ENV_SERVICE_NAME, Value: service},
		&common.EnvKeyValuePair{Name: ENV_MONITOR_COLLECT_INTERVAL, Value: fmt.Sprintf("%ds", opts.CollectIntervalSecs)},
		&common.EnvKeyValuePair{Name: ENV_MONITOR_SERVICE_NAME, Value: opts.MonitorServiceName},
		&common.EnvKeyValuePair{Name: ENV_MONITOR_SERVICE_TYPE, Value: attr.UserAttr.ServiceType},
		&common.EnvKeyValuePair{Name: ENV_MONITOR_SERVICE_MEMBERS, Value: members},
		&common.EnvKeyValuePair{Name: ENV_MONITOR_METRICS, Value: opts.MonitorMetrics},
	}

	for _, env := range serviceEnvs {
		envkvs = append(envkvs, env)
	}

	userAttr := &common.TGUserAttr{
		CollectIntervalSecs: opts.CollectIntervalSecs,
		MonitorServiceName:  opts.MonitorServiceName,
		MonitorMetrics:      opts.MonitorMetrics,
	}
	b, err := json.Marshal(userAttr)
	if err != nil {
		glog.Errorln("Marshal UserAttr error", err, opts)
		return nil, err
	}

	replicaCfgs := catalog.GenStatelessServiceReplicaConfigs(platform, cluster, service, 1)

	req := &manage.CreateServiceRequest{
		Service: &manage.ServiceCommonRequest{
			Region:      region,
			Cluster:     cluster,
			ServiceName: service,
			ServiceType: common.ServiceTypeStateless,
		},

		Resource: &common.Resources{
			MaxCPUUnits:     res.MaxCPUUnits,
			ReserveCPUUnits: res.ReserveCPUUnits,
			MaxMemMB:        res.MaxMemMB,
			ReserveMemMB:    res.ReserveMemMB,
		},

		ContainerImage: ContainerImage,
		// Telegraf only needs 1 container.
		Replicas: 1,
		Envkvs:   envkvs,
		// telegraf dockerfile expose 8125/udp 8092/udp 8094.
		// 8125 is the port that the statsD input plugin listens on.
		// 8092 is use by the udp listener plugin, and is deprecated in favor of the socket listener plugin.
		// 8094 the port that the socket listener plugin listens on, to listens for messages from
		// streaming (tcp, unix) or datagram (udp, unixgram) protocols.
		// Currently firecamp telegraf is only used to monitor the stateful service. So no need to expose
		// these ports and no need to register DNS for the telegraf service itself.
		RegisterDNS:    false,
		ReplicaConfigs: replicaCfgs,

		UserAttr: &common.ServiceUserAttr{
			ServiceType: common.CatalogService_Telegraf,
			AttrBytes:   b,
		},
	}
	return req, nil
}

// genServiceEnvs generates the envs for the service.
func genServiceEnvs(attr *common.ServiceAttr) (envkvs []*common.EnvKeyValuePair, err error) {
	switch attr.UserAttr.ServiceType {
	case common.CatalogService_Redis:
		ua := &common.RedisUserAttr{}
		err = json.Unmarshal(attr.UserAttr.AttrBytes, ua)
		if err != nil {
			return envkvs, err
		}
		envkvs = []*common.EnvKeyValuePair{
			&common.EnvKeyValuePair{Name: ENV_REDIS_AUTH, Value: ua.AuthPass},
		}
		return envkvs, nil

	default:
		return envkvs, nil
	}
}
