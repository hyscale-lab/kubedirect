#! /usr/bin/env bash

BASE_DIR=`realpath $(dirname $0)`
ROOT_DIR=$BASE_DIR/..
. $BASE_DIR/util.sh

set -x

# kubernetes 1.32.0-1.1 + docker 27.3.1 on ubuntu 22.04
function install_k8s {
    sudo apt-get update

    sudo ufw disable
    sudo apt-get install -y selinux-utils jq
    setenforce 0

    sudo swapoff -a
    sudo sed -ri 's/.*swap.*/#&/' /etc/fstab
    sudo systemctl mask swap.target

    sudo tee /etc/modules-load.d/containerd.conf <<EOF >/dev/null
overlay
br_netfilter
EOF

    sudo modprobe overlay
    sudo modprobe br_netfilter

    sudo tee /etc/sysctl.d/99-k8s.conf <<EOF >/dev/null
net.bridge.bridge-nf-call-iptables  = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward                 = 1
EOF

    # increase ulimits for users
    sudo tee /etc/security/limits.d/99-kd.conf <<EOF >/dev/null
* soft nofile 65535
* hard nofile 65535
root soft nofile 65535
root hard nofile 65535
* soft nproc 65535
* hard nproc 65535
root soft nproc 65535
root hard nproc 65535
* soft core unlimited
* hard core unlimited
root soft core unlimited
root hard core unlimited
EOF
    
    sudo sysctl --system

    # install docker & containerd
    sudo apt-get -y install apt-transport-https ca-certificates curl software-properties-common wget gnupg

    sudo curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
    sudo chmod a+r /etc/apt/keyrings/docker.asc

    echo \
    "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu \
    $(. /etc/os-release && echo "$VERSION_CODENAME") stable" | \
    sudo tee /etc/apt/sources.list.d/docker.list >/dev/null

    sudo apt-get update
    sudo apt-get -y install docker-ce=5:27.3.1-1~ubuntu.22.04~jammy docker-ce-cli=5:27.3.1-1~ubuntu.22.04~jammy containerd.io

    # configure docker
    DOCKER_ROOT=${DOCKER_ROOT:-"/workspace/docker"}
    sudo mkdir -p $DOCKER_ROOT
    sudo chown -R $(whoami) $DOCKER_ROOT
    sudo tee /etc/docker/daemon.json <<EOF >/dev/null
{
    "data-root": "$DOCKER_ROOT"
}
EOF

    # increase ulimits for systemd docker.service
    sudo mkdir -p /etc/systemd/system/docker.service.d
    sudo tee /etc/systemd/system/docker.service.d/override.conf <<EOF >/dev/null
[Service]
LimitNPROC=infinity
LimitCORE=infinity
LimitNOFILE=infinity
EOF

    sudo systemctl daemon-reload
    sudo systemctl enable docker
    sudo systemctl restart docker

    # docker permissions
    sudo apt-get -y install acl
    sudo usermod -aG docker $USER
    sudo setfacl -m "user:$USER:rw" /var/run/docker.sock

    # containerd
    # increase ulimits for systemd containerd.service
    sudo mkdir -p /etc/systemd/system/containerd.service.d
    sudo tee /etc/systemd/system/containerd.service.d/override.conf <<EOF >/dev/null
[Service]
LimitNPROC=infinity
LimitCORE=infinity
LimitNOFILE=infinity
EOF

    # configure containerd
    containerd config default | sudo tee /etc/containerd/config.toml >/dev/null 2>&1
    
    # remove rlimits from containerd cri base spec
    # NOTE: this only affects containers run from cri, not docker or ctr
    ctr oci spec | jq 'del(.process.rlimits)' --indent 4 | sudo tee /etc/containerd/cri-base.json >/dev/null
    sudo sed -i 's|base_runtime_spec = ""|base_runtime_spec = "/etc/containerd/cri-base.json"|g' /etc/containerd/config.toml
    
    # override pause and use systemd cgroup
    sudo sed -i 's|sandbox_image = "registry.k8s.io/pause:3.8"|sandbox_image = "shengqipku/pause:3.10"|g' /etc/containerd/config.toml
    sudo sed -i 's|SystemdCgroup = false|SystemdCgroup = true|g' /etc/containerd/config.toml

    sudo systemctl daemon-reload
    sudo systemctl enable containerd
    sudo systemctl restart containerd

    # install k8s
    curl -fsSL https://pkgs.k8s.io/core:/stable:/v1.32/deb/Release.key | sudo gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg --yes
    sudo chmod 644 /etc/apt/keyrings/kubernetes-apt-keyring.gpg # allow unprivileged APT programs to read this keyring

    echo 'deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v1.32/deb/ /' | sudo tee /etc/apt/sources.list.d/kubernetes.list
    sudo chmod 644 /etc/apt/sources.list.d/kubernetes.list >/dev/null

    sudo apt-get update
    sudo apt-get install -y kubelet=1.32.0-1.1 kubeadm=1.32.0-1.1 kubectl=1.32.0-1.1
    sudo apt-mark hold kubelet kubeadm kubectl
    echo 'source <(kubectl completion bash)' >>~/.bashrc
    source ~/.bashrc

    sudo crictl config --set runtime-endpoint=unix:///run/containerd/containerd.sock

    sudo apt-get install -y python3-pip
    python3 -m pip install --upgrade pip
    pip3 install --upgrade pyyaml
    pip3 install parse numpy matplotlib scipy pandas

    sudo apt-get install -y git-lfs
    git lfs install

    sudo sysctl --system
}

function install_go {
    curl -LO https://go.dev/dl/go1.23.0.linux-amd64.tar.gz
    sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.23.0.linux-amd64.tar.gz
    rm go1.23.0.linux-amd64.tar.gz
    echo 'export PATH=$PATH:/usr/local/go/bin' >>~/.bashrc
    source ~/.bashrc
}

function install_kind {
    curl -Lo ./kind https://kind.sigs.k8s.io/dl/v0.27.0/kind-linux-amd64
    chmod +x ./kind
    sudo mv ./kind /usr/local/bin/kind
}

function install_kwok {
    source ~/.bashrc
    # KWOK repository
    KWOK_REPO=kubernetes-sigs/kwok
    # Get latest
    KWOK_LATEST_RELEASE=$(curl "https://api.github.com/repos/${KWOK_REPO}/releases/latest" | jq -r '.tag_name')
    # kwokctl
    wget -O kwokctl -c "https://github.com/${KWOK_REPO}/releases/download/${KWOK_LATEST_RELEASE}/kwokctl-$(go env GOOS)-$(go env GOARCH)"
    chmod +x kwokctl
    sudo mv kwokctl /usr/local/bin/kwokctl
    # kwok
    wget -O kwok -c "https://github.com/${KWOK_REPO}/releases/download/${KWOK_LATEST_RELEASE}/kwok-$(go env GOOS)-$(go env GOARCH)"
    chmod +x kwok
    sudo mv kwok /usr/local/bin/kwok
}