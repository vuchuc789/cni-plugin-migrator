#!/usr/bin/env bash

helm repo add cilium https://helm.cilium.io/
helm upgrade --install cilium cilium/cilium --version 1.15.6 --namespace kube-system --values values-final.yaml
