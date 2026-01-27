# Kubedirect (Kd)
==================================

This repository includes the artifacts of our NSDI '26 paper "KUBEDIRECT: Unleashing the Full Power of the Cluster Manager for Serverless Computing".

## Overview

Kubedirect (Kd) is an optimized Kubernetes (K8s) release that employs direct message passing and lightweight opportunistic state management between stable components in the control plane.

The structure of this repository is as follows:

- `kubernetes`: git submodule containing our modified K8s codebase.
- `kubedirect`: Kd common interfaces and utilities.
- `pkg`: benchmarking utilities.
- `experiments`: experiment suites, including `microbench` and `trace`.

## Environment Setup

Our experiments are conducted on CloudLab xl170 nodes with Ubuntu 22.04. We have reserved a 20-node cluster for the convenience of reproduction. Please contact the authors for access.

If you wish to set up your own cluster, please refer to the [CloudLab manual](https://www.cloudlab.us/).
***Reviewers using the reserved cluster can skip the following steps***.

After setting up the cluster, you will have to upload your ssh private key, i.e., the one that matches your registered public key on CloudLab, to your master node as `~/.ssh/id_rsa`. This is how we the configures the cluster in a centralized manner from the master node.

Next, on the master node, clone the repository with

```bash
git clone --recursive https://github.com/TomQuartz/kubedirect-ae.git
```

then run 

```bash
./scripts/setup.sh
```

to install necessary dependencies and set up SSH across all nodes.
Restart the terminal session after running the script.


## Experiments

**NOTE**: we can only reserve 20 nodes due to resource contention on CloudLab, which is 4$\times$ smaller than the cluster used in the paper. As a result, we have also shrinked the size of our experiments accordingly. The absolute numbers in the reproduced results may differ from those in the paper. However, the overall trends should remain consistent.

### Microbenchmarks

`experiments/microbench` corresponds to Figure 9--11 of the paper. We provide an all-in-one script `all.sh` to run the entire microbenchmark suite. Inside the directory, run

```bash
./all.sh ${ID}
```

with `${ID}` the identifier of this experiment run.
It internally calls `scale_pods.sh` (Figure 9), `scale_funcs.sh` (Figure 10) and `scale_nodes.sh` (Figure 11). The raw experiment logs will be stored in `results/${bench}/${ID}`, where `${bench}` can be `scale-pods`, `scale-funcs` or `scale-nodes`.

For the convenience of reproduction, our scripts can directly generate plots from the results if run to completion. You can find them in the same spot as the raw logs.

Each run of `all.sh` should take around 3 hours to complete. `scale_pods.sh` should run for 40 minutes, `scale_funcs.sh` for 1 hour, and `scale_nodes.sh` for 1.5 hours.

### Azure Functions Trace

`experiments/trace` corresponds to Figure 12--13 of the paper. Like the microbenchmarks, we provide an all-in-one script `all.sh` to run the entire trace suite. Inside the directory, run

```bash
./download.sh # if ./data folder is not present
./all.sh ${ID}
```

to produce the results of `Kn/Kd` (Figure 12) and `Dr/K8s+`, `Dr/Kd+` (Figure 13). We obtain the results of `Kn/K8s` (Figure 12) and `Dirigent` (Figure 13) following the instructions of our primary baseline [Dirigent](https://github.com/eth-easl/dirigent). 

Because Dirigent's setup can be quite complicated, e.g., reloading the node images, and takes at least an hour to complete, we do not automate its execution in our scripts.
Instead, we include the Dirirent experiment logs collected during the submission of this paper, in `results/dirigent/default` (`Dirigent`) and `results/k8s/default` (`Kn/K8s`).
For other baselines, the raw logs can be found at `results/${bench}/${ID}`, where `${bench}` can be `kd`, `k8s+` or `kd+`.

>NOTE
>The results of `Dirigent` and `Kn/K8s` are obtained from the original 80-node cluster, while the results of `Kn/Kd`, `Dr/K8s+` and `Dr/Kd+` are obtained from the 20-node cluster. Therefore `Dirigent` and `Kn/K8s` should perform relatively better, but Kd-variants should still consistently outperform K8s-variants and have small gaps with `Dirigent`. 

For the convenience of reproduction, `all.sh` can directly generate plots from the results if run to completion. You can find them under `results/figures/${ID}`.

Each run of `all.sh` should take at 2 hours to complete.

## Troubleshooting

Our scripts automatically clean up K8s/Kd components after each experiment run. However, the cluster may not be properly cleaned up in case of keyboard interruptions or other unexpected errors. You can manually clean up the cluster by running the following command on the *master* node:

```bash
./scripts/kubelet.sh clean
./scripts/kubeadm.sh clean
```

Also note that concurrent experiment runs will interfere with each other. We use `flock` in the entrypoint scripts, i.e., `all.sh`, to prevent this. The child scripts are NOT intended to be run directly.

## License

Distributed under the MIT License. See `LICENSE`.
