package manageserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/golang/glog"
	"golang.org/x/net/context"

	"github.com/cloudstax/firecamp/catalog/redis"
	"github.com/cloudstax/firecamp/common"
	"github.com/cloudstax/firecamp/db"
	"github.com/cloudstax/firecamp/manage"
)

func (s *ManageHTTPServer) createRedisService(ctx context.Context, r *http.Request, requuid string) (errmsg string, errcode int) {
	// parse the request
	req := &manage.CatalogCreateRedisRequest{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		glog.Errorln("CatalogCreateRedisRequest decode request error", err, "requuid", requuid)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	err = s.checkRequest(req.Service, req.Resource)
	if err != nil {
		glog.Errorln("CatalogCreateRedisRequest invalid request, local cluster", s.cluster,
			"region", s.region, "requuid", requuid, req.Service, "error", err)
		return err.Error(), http.StatusBadRequest
	}

	glog.Infoln("create redis service", req.Service, req.Options, req.Resource)

	err = rediscatalog.ValidateRequest(req)
	if err != nil {
		glog.Errorln("CatalogCreateRedisRequest parameters are not valid, requuid", requuid, req.Service, req.Options)
		return err.Error(), http.StatusBadRequest
	}

	// create the service in the control plane and the container platform
	crReq, err := rediscatalog.GenDefaultCreateServiceRequest(s.platform, s.region, s.azs, s.cluster,
		req.Service.ServiceName, req.Resource, req.Options)
	if err != nil {
		glog.Errorln("create redis service request error", err, "requuid", requuid, req.Service)
		return manage.ConvertToHTTPError(err)
	}

	// create the service in the control plane
	serviceUUID, err := s.svc.CreateService(ctx, crReq, s.domain, s.vpcID)
	if err != nil {
		glog.Errorln("create service error", err, "requuid", requuid, req.Service)
		return manage.ConvertToHTTPError(err)
	}

	glog.Infoln("created Redis service in the control plane", serviceUUID, "requuid", requuid, req.Service, req.Options)

	err = s.updateRedisStaticIPs(ctx, serviceUUID, requuid)
	if err != nil {
		glog.Errorln("updateRedisStaticIPs error", err, "requuid", requuid, req.Service)
		return manage.ConvertToHTTPError(err)
	}

	err = s.createContainerService(ctx, crReq, serviceUUID, requuid)
	if err != nil {
		glog.Errorln("createContainerService error", err, "requuid", requuid, req.Service)
		return manage.ConvertToHTTPError(err)
	}

	glog.Infoln("created Redis service", serviceUUID, "requuid", requuid, req.Service, req.Options)

	if rediscatalog.IsClusterMode(req.Options.Shards) {
		glog.Infoln("The cluster mode Redis is created, add the init task, requuid", requuid, req.Service, req.Options)

		// for Redis cluster mode, run the init task in the background
		err = s.addRedisInitTask(ctx, crReq.Service, serviceUUID, req.Options.Shards, req.Options.ReplicasPerShard, requuid)
		if err != nil {
			glog.Errorln("addRedisInitTask error", err, "requuid", requuid, req.Service)
			return manage.ConvertToHTTPError(err)
		}

		return "", http.StatusOK
	}

	// redis single instance or master-slave mode does not require additional init work. set service initialized
	glog.Infoln("created Redis service", serviceUUID, "requuid", requuid, req.Service, req.Options)

	return s.setServiceInitialized(ctx, req.Service.ServiceName, requuid)
}

func (s *ManageHTTPServer) addRedisInitTask(ctx context.Context, req *manage.ServiceCommonRequest,
	serviceUUID string, shards int64, replicasPerShard int64, requuid string) error {
	logCfg := s.logIns.CreateStreamLogConfig(ctx, s.cluster, req.ServiceName, serviceUUID, common.TaskTypeInit)

	taskOpts, err := rediscatalog.GenDefaultInitTaskRequest(req, logCfg, shards, replicasPerShard, serviceUUID, s.manageurl)
	if err != nil {
		return err
	}

	task := &serviceTask{
		serviceUUID: serviceUUID,
		serviceName: req.ServiceName,
		serviceType: common.CatalogService_Redis,
		opts:        taskOpts,
	}

	s.catalogSvcInit.addInitTask(ctx, task)

	glog.Infoln("add init task for Redis service", serviceUUID, "requuid", requuid, req)
	return nil
}

func (s *ManageHTTPServer) updateRedisService(ctx context.Context, r *http.Request, requuid string) (errmsg string, errcode int) {
	// parse the request
	req := &manage.CatalogUpdateRedisRequest{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		glog.Errorln("CatalogUpdateRedisRequest decode request error", err, "requuid", requuid)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	err = s.checkCommonRequest(req.Service)
	if err != nil {
		glog.Errorln("CatalogUpdateRedisRequest invalid request, local cluster", s.cluster,
			"region", s.region, "requuid", requuid, req.Service, "error", err)
		return err.Error(), http.StatusBadRequest
	}

	err = rediscatalog.ValidateUpdateRequest(req)
	if err != nil {
		glog.Errorln("ValidateUpdateRequest error", err, "requuid", requuid, req.Service)
		return err.Error(), http.StatusBadRequest
	}

	svc, err := s.dbIns.GetService(ctx, s.cluster, req.Service.ServiceName)
	if err != nil {
		glog.Errorln("GetService error", err, "requuid", requuid, req.Service)
		return manage.ConvertToHTTPError(err)
	}

	glog.Infoln("update redis service configs, requuid", requuid, svc, req)

	err = s.updateRedisConfigs(ctx, svc.ServiceUUID, req, requuid)
	if err != nil {
		glog.Errorln("updateRedisConfigs error", err, "requuid", requuid, req.Service, req)
		return manage.ConvertToHTTPError(err)
	}

	return "", http.StatusOK
}

func (s *ManageHTTPServer) updateRedisConfigs(ctx context.Context, serviceUUID string, req *manage.CatalogUpdateRedisRequest, requuid string) error {
	attr, err := s.dbIns.GetServiceAttr(ctx, serviceUUID)
	if err != nil {
		glog.Errorln("GetServiceAttr error", err, "requuid", requuid, req.Service)
		return err
	}

	ua := &common.RedisUserAttr{}
	if attr.UserAttr != nil {
		// sanity check
		if attr.UserAttr.ServiceType != common.CatalogService_Redis {
			glog.Errorln("not a redis service", attr.UserAttr.ServiceType, "requuid", requuid, req.Service)
			return errors.New("the service is not a redis service")
		}

		err = json.Unmarshal(attr.UserAttr.AttrBytes, ua)
		if err != nil {
			glog.Errorln("Unmarshal UserAttr error", err, "requuid", requuid, req.Service)
			return err
		}
	}

	if !rediscatalog.IsConfigChanged(ua, req) {
		glog.Infoln("redis attr is not changed, requuid", requuid, req.Service)
		return nil
	}

	err = s.updateRedisMemberConfigs(ctx, serviceUUID, ua, req, requuid)
	if err != nil {
		glog.Errorln("updateRedisMemberConfigs error", err, "requuid", requuid, req.Service)
		return err
	}

	newua := rediscatalog.UpdateRedisUserAttr(ua, req)
	b, err := json.Marshal(newua)
	if err != nil {
		glog.Errorln("Marshal user attr error", err, "requuid", requuid, req.Service)
		return err
	}
	userAttr := &common.ServiceUserAttr{
		ServiceType: common.CatalogService_Redis,
		AttrBytes:   b,
	}

	newAttr := db.UpdateServiceUserAttr(attr, userAttr)
	err = s.dbIns.UpdateServiceAttr(ctx, attr, newAttr)
	if err != nil {
		glog.Errorln("UpdateServiceAttr error", err, "requuid", requuid, req.Service)
		return err
	}

	glog.Infoln("updated service configs from", ua, "to", newua, "requuid", requuid, req.Service)

	return nil
}

func (s *ManageHTTPServer) getRedisConfFile(member *common.ServiceMember, requuid string) (cfgIndex int, cfg *common.MemberConfig, err error) {
	cfgIndex = -1
	for i, c := range member.Configs {
		if rediscatalog.IsRedisConfFile(c.FileName) {
			cfg = c
			cfgIndex = i
			break
		}
	}
	if cfgIndex == -1 {
		errmsg := fmt.Sprintf("the redis config file not found for member %s, requuid %s", member.MemberName, requuid)
		return -1, nil, errors.New(errmsg)
	}
	return cfgIndex, cfg, nil
}

func (s *ManageHTTPServer) updateRedisMemberConfigs(ctx context.Context, serviceUUID string, ua *common.RedisUserAttr, req *manage.CatalogUpdateRedisRequest, requuid string) error {
	members, err := s.dbIns.ListServiceMembers(ctx, serviceUUID)
	if err != nil {
		glog.Errorln("ListServiceMembers failed", err, "requuid", requuid, req.Service)
		return err
	}

	for _, member := range members {
		cfgIndex, cfg, err := s.getRedisConfFile(member, requuid)
		if err != nil {
			glog.Errorln(err)
			return err
		}

		// fetch the config file
		cfgfile, err := s.dbIns.GetConfigFile(ctx, member.ServiceUUID, cfg.FileID)
		if err != nil {
			glog.Errorln("GetConfigFile error", err, "requuid", requuid, cfg, member)
			return err
		}

		// replace the original member redis conf file content
		newContent := rediscatalog.UpdateRedisConfig(cfgfile.Content, ua, req)
		_, err = s.updateMemberConfig(ctx, member, cfgfile, cfgIndex, newContent, requuid)
		if err != nil {
			glog.Errorln("updateMemberConfig error", err, "requuid", requuid, cfg, member)
			return err
		}

		glog.Infoln("updated redis conf for member", member, "requuid", requuid)
	}

	glog.Infoln("updated redis conf for service", serviceUUID, "requuid", requuid)
	return nil
}

func (s *ManageHTTPServer) setRedisInit(ctx context.Context, r *http.Request, requuid string) (errmsg string, errcode int) {
	// parse the request
	req := &manage.CatalogSetRedisInitRequest{}
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		glog.Errorln("CatalogSetRedisInitRequest decode request error", err, "requuid", requuid)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	if req.Cluster != s.cluster || req.Region != s.region {
		glog.Errorln("CatalogSetRedisInitRequest invalid request, local cluster", s.cluster,
			"region", s.region, "requuid", requuid, req)
		return http.StatusText(http.StatusBadRequest), http.StatusBadRequest
	}

	glog.Infoln("setRedisInit", req.ServiceName, "requuid", requuid)

	// get service uuid
	service, err := s.dbIns.GetService(ctx, s.cluster, req.ServiceName)
	if err != nil {
		glog.Errorln("GetService", req, "error", err, "requuid", requuid)
		return manage.ConvertToHTTPError(err)
	}

	// get service attr
	attr, err := s.dbIns.GetServiceAttr(ctx, service.ServiceUUID)
	if err != nil {
		glog.Errorln("GetServiceAttr error", err, "requuid", requuid, service)
		return manage.ConvertToHTTPError(err)
	}

	// enable redis auth
	statusMsg := "enable redis auth"
	s.catalogSvcInit.UpdateTaskStatusMsg(service.ServiceUUID, statusMsg)
	err = s.enableRedisAuth(ctx, service.ServiceUUID, requuid)
	if err != nil {
		glog.Errorln("enableRedisAuth error", err, "requuid", requuid, service)
		return manage.ConvertToHTTPError(err)
	}

	// the config files of all replicas are updated, restart all containers
	glog.Infoln("all replicas are updated, restart all containers, requuid", requuid, req)

	// update the init task status message
	statusMsg = "restarting all containers"
	s.catalogSvcInit.UpdateTaskStatusMsg(service.ServiceUUID, statusMsg)

	// restart service containers
	err = s.containersvcIns.StopService(ctx, s.cluster, req.ServiceName)
	if err != nil {
		glog.Errorln("StopService error", err, "requuid", requuid, req)
		return manage.ConvertToHTTPError(err)
	}
	err = s.containersvcIns.ScaleService(ctx, s.cluster, req.ServiceName, attr.Replicas)
	if err != nil {
		glog.Errorln("ScaleService error", err, "requuid", requuid, req)
		return manage.ConvertToHTTPError(err)
	}

	// set service initialized
	glog.Infoln("all containers restarted, set service initialized, requuid", requuid, req)

	return s.setServiceInitialized(ctx, req.ServiceName, requuid)
}

func (s *ManageHTTPServer) updateRedisStaticIPs(ctx context.Context, serviceUUID string, requuid string) error {
	// update the redis member's cluster-announce-ip to the assigned static ip in redis.conf
	members, err := s.dbIns.ListServiceMembers(ctx, serviceUUID)
	if err != nil {
		glog.Errorln("ListServiceMembers failed", err, "serviceUUID", serviceUUID, "requuid", requuid)
		return err
	}

	for _, member := range members {
		cfgIndex, cfg, err := s.getRedisConfFile(member, requuid)
		if err != nil {
			glog.Errorln(err)
			return err
		}

		// fetch the config file
		cfgfile, err := s.dbIns.GetConfigFile(ctx, member.ServiceUUID, cfg.FileID)
		if err != nil {
			glog.Errorln("GetConfigFile error", err, "requuid", requuid, cfg, member)
			return err
		}

		// if static ip is already set, return
		setIP := rediscatalog.NeedToSetClusterAnnounceIP(cfgfile.Content)
		if !setIP {
			glog.Infoln("cluster-announce-ip is already set in the config file", db.PrintConfigFile(cfgfile), "requuid", requuid, member)
			return nil
		}

		// cluster-announce-ip not set, set it
		newContent := rediscatalog.SetClusterAnnounceIP(cfgfile.Content, member.StaticIP)

		_, err = s.updateMemberConfig(ctx, member, cfgfile, cfgIndex, newContent, requuid)
		if err != nil {
			glog.Errorln("updateMemberConfig error", err, "requuid", requuid, member)
			return err
		}
	}

	glog.Infoln("updated redis cluster-announce-ip to the static ip", serviceUUID, "requuid", requuid)
	return nil
}

func (s *ManageHTTPServer) enableRedisAuth(ctx context.Context, serviceUUID string, requuid string) error {
	// enable auth in redis.conf
	members, err := s.dbIns.ListServiceMembers(ctx, serviceUUID)
	if err != nil {
		glog.Errorln("ListServiceMembers failed", err, "serviceUUID", serviceUUID, "requuid", requuid)
		return err
	}

	for _, member := range members {
		cfgIndex, cfg, err := s.getRedisConfFile(member, requuid)
		if err != nil {
			glog.Errorln(err)
			return err
		}

		// fetch the config file
		cfgfile, err := s.dbIns.GetConfigFile(ctx, member.ServiceUUID, cfg.FileID)
		if err != nil {
			glog.Errorln("GetConfigFile error", err, "requuid", requuid, cfg, member)
			return err
		}

		// if auth is enabled, return
		enableAuth := rediscatalog.NeedToEnableAuth(cfgfile.Content)
		if !enableAuth {
			glog.Infoln("auth is already enabled in the config file", db.PrintConfigFile(cfgfile), "requuid", requuid, member)
			return nil
		}

		// auth is not enabled, enable it
		newContent := rediscatalog.EnableRedisAuth(cfgfile.Content)

		_, err = s.updateMemberConfig(ctx, member, cfgfile, cfgIndex, newContent, requuid)
		if err != nil {
			glog.Errorln("updateMemberConfig error", err, "requuid", requuid, member)
			return err
		}
	}

	glog.Infoln("enabled redis auth, serviceUUID", serviceUUID, "requuid", requuid)
	return nil
}
