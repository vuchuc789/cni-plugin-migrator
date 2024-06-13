#!/usr/bin/env bash

helm repo add cilium https://helm.cilium.io/
helm pull cilium/cilium --untar --untardir .

cilium install --chart-directory ./cilium --values values-migration.yaml --dry-run-helm-values > values-initial.yaml

cilium install --chart-directory ./cilium --values values-initial.yaml --dry-run-helm-values --set operator.unmanagedPodWatcher.restart=true --set cni.customConf=false --set policyEnforcementMode=default --set bpf.hostLegacyRouting=false > values-final.yaml # optional, can cause brief interruptions
