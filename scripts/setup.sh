#! /usr/bin/env bash

BASE_DIR=`realpath $(dirname $0)`
ROOT_DIR=$BASE_DIR/..
. $BASE_DIR/install.sh

set -x

function setup_ssh {
    mkdir -p ~/.ssh && rm -f ~/.ssh/config

    echo "Setting up ssh for the following nodes:" $(hosts)
    for host in $(hosts); do
        addr=`grep -w $host /etc/hosts | awk '{print $1}'`
        tee -a ~/.ssh/config <<EOF
Host $host
    Hostname $addr
    User $USER
    StrictHostKeyChecking no
    UserKnownHostsFile /dev/null
EOF
    done

    if [ ! -f ~/.ssh/id_rsa ]; then
        echo "please upload your key to ~/.ssh/id_rsa"
    fi

    chmod 600 ~/.ssh/config && chmod 700 ~/.ssh

    for host in $(hosts); do
    (
        scp ~/.ssh/id_rsa $host:~/.ssh/ # >/dev/null 2>&1
        ssh -q $host -- chmod 600 ~/.ssh/id_rsa
        ssh -q $host -- sudo hostnamectl set-hostname $host 
    ) &
    done
    wait
}

function setup_scripts {
    echo "Copying scripts across cluster..."
    for host in $(hosts); do
    (
        ssh -q $host -- rm -rf ~/.kubedirect
        ssh -q $host -- mkdir -p ~/.kubedirect
        scp -r $BASE_DIR $host:~/.kubedirect
    ) &
    done
    wait
}

function setup_k8s {
   install_k8s
}

function setup_install {
    install_go
    install_kind
    # install_kwok
    echo "Installing k8s across cluster..."
    for host in $(hosts); do
        ssh $host -- "~/.kubedirect/scripts/setup.sh k8s" &
    done
    wait

    # build and distribute kubelet binary
    git submodule update --init --recursive
    $BASE_DIR/build.sh kubelet
}

function setup_tmux {
    echo "set -g mouse on" > ~/.tmux.conf
    tmux source ~/.tmux.conf
}

function setup_all {
    setup_ssh
    setup_scripts
    setup_install
    setup_tmux
}

function setup_reboot {
    setup_ssh
    echo "Resetting configurations after reboot..."
    for host in $(hosts); do
        ssh -q $host -- <<EOF &
            sudo ufw disable
            sudo setenforce 0
            sudo swapoff -a
            sudo sed -ri 's/.*swap.*/#&/' /etc/fstab
            sudo systemctl mask swap.target
            sudo modprobe overlay
            sudo modprobe br_netfilter
            sudo sysctl --system
            sudo usermod -aG docker $USER
            sudo systemctl restart docker.service docker.socket
            sudo setfacl -m "user:$USER:rw" /var/run/docker.sock
EOF
    done
    wait
}

case "$1" in
ssh)
    setup_ssh
    ;;
scripts)
    setup_scripts
    ;;
k8s)
    setup_k8s
    ;;
install)
    setup_install
    ;;
all)
    setup_all
    ;;
reboot)
    setup_reboot
    ;;
*)
    echo "Usage: $0 {ssh|scripts|k8s|install|all|reboot}"
    exit 1
esac