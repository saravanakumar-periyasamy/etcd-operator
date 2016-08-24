package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"time"

	"github.com/coreos/etcd/clientv3"
	"golang.org/x/net/context"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/labels"
)

type clusterEventType string

const (
	eventNewCluster    clusterEventType = "Add"
	eventDeleteCluster clusterEventType = "Delete"
	eventReconcile     clusterEventType = "Reconcile"
)

type clusterEvent struct {
	typ          clusterEventType
	size         int
	antiAffinity bool
	// currently running pods in kubernetes
	running MemberSet
}

type Cluster struct {
	kclient *unversioned.Client

	antiAffinity bool

	name string

	idCounter int
	eventCh   chan *clusterEvent
	stopCh    chan struct{}

	// members repsersents the members in the etcd cluster.
	// the name of the member is the the name of the pod the member
	// process runs in.
	members MemberSet

	backupDir string
}

func newCluster(kclient *unversioned.Client, name string) *Cluster {
	c := &Cluster{
		kclient: kclient,
		name:    name,
		eventCh: make(chan *clusterEvent, 100),
		stopCh:  make(chan struct{}),
	}
	go c.run()

	return c
}

func (c *Cluster) init(spec Spec) {
	c.send(&clusterEvent{
		typ:          eventNewCluster,
		size:         spec.Size,
		antiAffinity: spec.AntiAffinity,
	})
}

func (c *Cluster) Delete() {
	c.send(&clusterEvent{typ: eventDeleteCluster})
}

func (c *Cluster) send(ev *clusterEvent) {
	select {
	case c.eventCh <- ev:
	case <-c.stopCh:
	default:
		panic("TODO: too many events queued...")
	}
}

func (c *Cluster) run() {
	go c.monitorPods()
	for {
		select {
		case event := <-c.eventCh:
			switch event.typ {
			case eventNewCluster:
				c.create(event.size, event.antiAffinity)
			case eventReconcile:
				if err := c.reconcile(event.running); err != nil {
					panic(err)
				}
			case eventDeleteCluster:
				c.delete()
				close(c.stopCh)
				return
			}
		}
	}
}

func (c *Cluster) create(size int, antiAffinity bool) {
	c.antiAffinity = antiAffinity

	members := MemberSet{}
	// we want to make use of member's utility methods.
	for i := 0; i < size; i++ {
		etcdName := fmt.Sprintf("%s-%04d", c.name, i)
		members.Add(&Member{Name: etcdName})
	}

	// TODO: parallelize it
	for i := 0; i < size; i++ {
		etcdName := fmt.Sprintf("%s-%04d", c.name, i)
		if err := c.createPodAndService(members, members[etcdName], "new"); err != nil {
			panic(fmt.Sprintf("(TODO: we need to clean up already created ones.)\nError: %v", err))
		}
		c.idCounter++
	}

	fmt.Println("created cluster:", members)
}

func (c *Cluster) updateMembers(etcdcli *clientv3.Client) {
	resp, err := etcdcli.MemberList(context.TODO())
	if err != nil {
		panic(err)
	}
	c.members = MemberSet{}
	for _, m := range resp.Members {
		id := findID(m.Name)
		if id+1 > c.idCounter {
			c.idCounter = id + 1
		}

		c.members[m.Name] = &Member{
			Name: m.Name,
			ID:   m.ID,
		}
	}
}
func findID(name string) int {
	var id int
	fmt.Sscanf(name, "etcd-cluster-%d", &id)
	return id
}
func (c *Cluster) delete() {
	option := api.ListOptions{
		LabelSelector: labels.SelectorFromSet(map[string]string{
			"etcd_cluster": c.name,
		}),
	}

	pods, err := c.kclient.Pods("default").List(option)
	if err != nil {
		panic(err)
	}
	for i := range pods.Items {
		if err := c.removePodAndService(pods.Items[i].Name); err != nil {
			panic(err)
		}
	}
}

// todo: use a struct to replace the huge arg list.
func (c *Cluster) createPodAndService(members MemberSet, m *Member, state string) error {
	if err := createEtcdService(c.kclient, m.Name, c.name); err != nil {
		return err
	}
	return createEtcdPod(c.kclient, members.PeerURLPairs(), m, c.name, state, c.antiAffinity)
}

func (c *Cluster) removePodAndService(name string) error {
	err := c.kclient.Pods("default").Delete(name, nil)
	if err != nil {
		if !isKubernetesResourceNotFoundError(err) {
			return err
		}
	}
	err = c.kclient.Services("default").Delete(name)
	if err != nil {
		if !isKubernetesResourceNotFoundError(err) {
			return err
		}
	}
	return nil
}

func (c *Cluster) backup() error {
	clientAddr := "todo"
	nextSnapshotName := "todo"

	cfg := clientv3.Config{
		Endpoints: []string{clientAddr},
	}
	etcdcli, err := clientv3.New(cfg)
	if err != nil {
		panic(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)

	log.Println("saving snapshot from cluster", c.name)

	rc, err := etcdcli.Maintenance.Snapshot(ctx)
	cancel()
	if err != nil {
		return err
	}

	tmpfile, err := ioutil.TempFile(c.backupDir, "snapshot")
	n, err := io.Copy(tmpfile, rc)
	if err != nil {
		tmpfile.Close()
		os.Remove(tmpfile.Name())
		log.Printf("saving snapshot from cluster %s error: %v\n", c.name, err)
		return err
	}

	err = os.Rename(tmpfile.Name(), nextSnapshotName)
	if err != nil {
		os.Remove(tmpfile.Name())
		log.Printf("renaming snapshot from cluster %s error: %v\n", c.name, err)
		return err
	}

	log.Printf("saved snapshot %v (size: %d) from cluster %s", n, nextSnapshotName, c.name)

	return nil
}

func (c *Cluster) monitorPods() {
	opts := api.ListOptions{
		LabelSelector: labels.SelectorFromSet(map[string]string{
			"etcd_cluster": c.name,
		}),
	}
	// TODO: Select "etcd_node" to remove left service.
	for {
		select {
		case <-c.stopCh:
			return
		case <-time.After(5 * time.Second):
		}

		podList, err := c.kclient.Pods("default").List(opts)
		if err != nil {
			panic(err)
		}
		running := MemberSet{}
		for i := range podList.Items {
			running.Add(&Member{Name: podList.Items[i].Name})
		}

		c.send(&clusterEvent{
			typ:     eventReconcile,
			running: running,
		})
	}
}