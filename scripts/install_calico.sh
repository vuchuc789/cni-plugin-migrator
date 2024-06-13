#!/usr/bin/env bash

kubectl get configmaps -n kube-system rke-network-plugin -o yaml | yq -r e '.data.rke-network-plugin' - > calico.yaml
kubectl apply -f calico.yaml
