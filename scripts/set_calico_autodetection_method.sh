#!/usr/bin/env bash

kubectl set env daemonset/calico-node -n kube-system IP_AUTODETECTION_METHOD=cidr=172.16.120.0/24
