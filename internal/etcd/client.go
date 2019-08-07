package etcd

import (
	"fmt"
	"time"

	"go.etcd.io/etcd/clientv3"

	etcdv1alpha1 "github.com/munnerz/etcd-operator/api/v1alpha1"
)

func ClientFor(peers ...etcdv1alpha1.EtcdPeer) (*clientv3.Client, error) {
	cl, err := clientv3.New(clientConfigForPeers(peers...))
	if err != nil {
		return nil, err
	}

	return cl, nil
}

func clientConfigForPeers(peers ...etcdv1alpha1.EtcdPeer) clientv3.Config {
	var ep []string
	for _, p := range peers {
		ep = append(ep, dnsNameForPeer(p))
	}
	return clientv3.Config{
		Endpoints:   ep,
		DialTimeout: time.Second * 5,
	}
}

func dnsNameForPeer(peer etcdv1alpha1.EtcdPeer) string {
	// TODO: allow customising port and scheme
	return fmt.Sprintf("http://127.0.0.1:2379")
	//return fmt.Sprintf("http://%s.%s.%s:2379", peer.Name, peer.Spec.ClusterName, peer.Namespace)
}
