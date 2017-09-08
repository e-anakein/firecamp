#!/bin/bash -x

version=$1
clusterName=$2
containerPlatform=$3
containerPlatformRole=$4
azs=$5

if [ "$version" = "" -o "$clusterName" = "" -o "$azs" = "" ]; then
  echo "version $version, clusterName $clusterName and azs $azs should not be empty"
  exit 1
fi

if [ "$containerPlatform" != "ecs" -a "$containerPlatform" != "swarm" ]; then
  echo "invalid container platform $containerPlatform"
  exit 1
fi

if [ "$containerPlatformRole" != "worker" -a "$containerPlatformRole" != "manager" ]; then
  echo "invalid container platform role $containerPlatformRole"
  exit 1
fi

echo "init version $version, cluster $clusterName, container platform $containerPlatform, role $containerPlatformRole, azs $azs"

# 1. tune the system configs.
# set never for THP (Transparent Huge Pages), as THP might impact the performance for some services
# such as MongoDB and Reddis. Could set to madvise if some service really benefits from madvise.
# https://www.kernel.org/doc/Documentation/vm/transhuge.txt
echo never | sudo tee /sys/kernel/mm/transparent_hugepage/enabled

# increase somaxconn to 512 for such as Redis
echo "net.core.somaxconn=512" >> /etc/sysctl.conf
sysctl -w net.core.somaxconn=512
echo "net.ipv4.tcp_max_syn_backlog=512" >> /etc/sysctl.conf
sysctl -w net.ipv4.tcp_max_syn_backlog=512

# set overcommit to 1 as required by Redis. Would not cause issue to other services
echo "vm.overcommit_memory=1" >> /etc/sysctl.conf
sysctl -w vm.overcommit_memory=1


# 2. install docker.
# TODO install a specific version. sudo yum list docker --showduplicates. sudo yum install docker-17.03.1ce
# [ec2-user@ip-172-31-69-166 ~]$ sudo yum list docker --showduplicates
# Loaded plugins: priorities, update-motd, upgrade-helper
# Available Packages
# docker.x86_64                 1.12.6-2.19.amzn1                      amzn-main
# docker.x86_64                 17.03.1ce-1.50.amzn1                   amzn-updates
# docker.x86_64                 17.03.2ce-1.59.amzn1                   amzn-updates
yum install -y docker

# Kafka uses a very large number of files, increase the file descriptor count.
# AWS AMI sets the ulimit for docker daemon, OPTIONS=\"--default-ulimit nofile=1024:4096\".
# The container inherits the docker daemon ulimit.
# The docker daemon config file is different on different Linux. AWS AMI is /etc/sysconfig/docker.
# Ubuntu is /etc/init/docker.conf
sed -i 's/OPTIONS=\"--default-ulimit.*/OPTIONS=\"--default-ulimit nofile=100000:100000\"/g' /etc/sysconfig/docker

service docker start


# 3. Container platform specific initialization.
if [ "$containerPlatform" = "ecs" ]; then
  # install cloudstax ecs init
  wget -O /tmp/cloudstax-ecs-init-1.14.4-1.amzn1.x86_64.rpm https://s3.amazonaws.com/cloudstax/firecamp/releases/$version/packages/cloudstax-ecs-init-1.14.4-1.amzn1.x86_64.rpm
  rpm -ivh /tmp/cloudstax-ecs-init-1.14.4-1.amzn1.x86_64.rpm
  echo "ECS_CLUSTER=$clusterName" >> /etc/ecs/ecs.config
  start ecs

  # install firecamp docker volume plugin
  mkdir -p /var/log/firecamp
  docker plugin install --grant-all-permissions cloudstax/firecamp-volume:$version

  # TODO enable log driver after upgrade to 17.05/06
  # install firecamp docker log driver
  # docker plugin install cloudstax/firecamp-logs
fi

if [ "$containerPlatform" = "swarm" ]; then
  wget -O /tmp/firecamp-swarminit https://s3.amazonaws.com/cloudstax/firecamp/releases/$version/packages/firecamp-swarminit
  chmod +x /tmp/firecamp-swarminit
  # initialize the swarm node
  /tmp/firecamp-swarminit -cluster="$clusterName" -role="$containerPlatformRole" -availability-zones="$azs"
  if [ "$?" != "0" ]; then
    echo "ERROR: firecamp-swarminit failed"
    exit 2
  fi

  echo "firecamp-swarminit done"

  if [ "$containerPlatformRole" = "worker" ]; then
    # worker node, install the firecamp plugin
    # install firecamp docker volume plugin
    mkdir -p /var/log/firecamp
    docker plugin install --grant-all-permissions cloudstax/firecamp-volume:$version PLATFORM="swarm" CLUSTER="$clusterName"

    # TODO enable log driver after upgrade to 17.05/06
    # install firecamp docker log driver
    # docker plugin install cloudstax/firecamp-logs
  fi
fi

