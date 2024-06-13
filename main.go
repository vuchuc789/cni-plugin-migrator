package main

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

func main() {
	var (
		kubeconfig *string
		// rollback   *bool
	)

	ctx := context.TODO()

	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}

	// rollback = flag.Bool("rollback", false, "proceed rollback")

	flag.Parse()

	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	CheckIfError(err)

	clientset, err := kubernetes.NewForConfig(config)
	CheckIfError(err)

	nodesClient := clientset.CoreV1().Nodes()

	list, err := nodesClient.List(ctx, metav1.ListOptions{})
	CheckIfError(err)

	for _, node := range list.Items {
		fmt.Println(node.Annotations["io.cilium.migration/cilium-default"])
	}
}
