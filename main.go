package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/ankitforcode/awsutils"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/coreos/etcd/clientv3"
)

// Service :
type Service struct {
	instance       string
	clusterTagName string
}

type etcdState struct {
	Name       string         `json:"name"`
	ID         string         `json:"id"`
	State      string         `json:"state"`
	StartTime  time.Time      `json:"startTime"`
	LeaderInfo etcdLeaderInfo `json:"leaderInfo"`
}

type etcdLeaderInfo struct {
	Leader               string    `json:"leader"`
	Uptime               string    `json:"uptime"`
	StartTime            time.Time `json:"startTime"`
	RecvAppendRequestCnt int       `json:"recvAppendRequestCnt"`
	RecvPkgRate          int       `json:"recvPkgRate"`
	RecvBandwidthRate    int       `json:"recvBandwidthRate"`
	SendAppendRequestCnt int       `json:"sendAppendRequestCnt"`
}

type etcdMembers struct {
	Members []etcdMember `json:"members,omitempty"`
}

type etcdMember struct {
	ID         string   `json:"id,omitempty"`
	Name       string   `json:"name,omitempty"`
	PeerURLs   []string `json:"peerURLs,omitempty"`
	ClientURLs []string `json:"clientURLs,omitempty"`
}

var (
	localInstance         *ec2.Instance
	etcdPeerPort          string
	peerProtocol          string
	clientProtocol        string
	etcdClientPort        string
	etcdHeartbeatInterval uint
	etcdElectionTimeout   uint
)

func main() {
	svc := new(Service)
	flag.StringVar(&svc.instance, "instance", "", "The instance ID of the cluster member. If not supplied, then the instance ID is determined from EC2 metadata")
	flag.StringVar(&svc.clusterTagName, "clusterTagName", "aws:autoscaling:groupName", "The instance tag that is common to all members of the cluster")
	defaultLifecycleQueueName := ""
	if lq := os.Getenv("LIFECYCLE_QUEUE_NAME"); lq != "" {
		defaultLifecycleQueueName = lq
	}
	lifecycleQueueName := flag.String("lifecycle-queue-name", defaultLifecycleQueueName,
		"The name of the lifecycle SQS queue (optional). "+
			"Environment variable: LIFECYCLE_QUEUE_NAME")
	defaultPeerPort := "2380"
	if pp := os.Getenv("ETCD_PEER_PORT"); pp != "" {
		defaultPeerPort = pp
	}
	flag.StringVar(&etcdPeerPort, "etcd-peer-port", defaultPeerPort,
		"Etcd peer port number. "+
			"Environment variable: ETCD_PEER_PORT")
	flag.Parse()

	peerProtocol = "http"
	clientProtocol = "http"
	etcdClientPort = "2379"
	etcdElectionTimeout = 500
	etcdHeartbeatInterval = 100
	awsSession := session.New()
	if region := os.Getenv("AWS_REGION"); region != "" {
		awsSession.Config.WithRegion(region)
	}
	awsutils.GuessRegion(awsSession.Config)
	var err error
	if svc.instance == "" {
		svc.instance, err = awsutils.DiscoverInstanceID()
		if err != nil {
			log.Fatalf(err.Error())
		}
	}
	s := &awsutils.Cluster{
		AwsSession: awsSession,
		InstanceID: svc.instance,
		TagName:    svc.clusterTagName,
	}

	localInstance, err := s.Instance()
	if err != nil {
		return
	}
	initialClusterState, initialCluster, err := buildCluster(s)
	log.Printf("initial cluster: %s %s", initialClusterState, initialCluster)
	go func() {
		// wait for etcd to start
		var etcdClient *clientv3.Client
		for {
			log.Printf("etcd connecting")
			etcdClient, err = getEtcdClient([]string{fmt.Sprintf("%s://%s:%s",
				clientProtocol, *localInstance.PrivateIpAddress, etcdClientPort)})
			if err != nil {
				log.Fatalf("ERROR: %s", err)
			}
			defer etcdClient.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			err := etcdClient.Sync(ctx)
			cancel()
			if err != nil {
				log.Printf("waiting for etcd to start: %s", err)
			} else {
				log.Printf("etcd connected")
				resp, _ := etcdClient.MemberList(context.Background())
				log.Printf("etcd members: %s", resp.Members)
				break
			}
			time.Sleep(time.Second)
		}
	}()

	go watchLifecycleEvents(s, *lifecycleQueueName)

	cmd := exec.Command("etcd3")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = []string{
		fmt.Sprintf("ETCD_NAME=%s", *localInstance.InstanceId),
		fmt.Sprintf("ETCD_DATA_DIR=%s", "/var/lib/etcd"),
		fmt.Sprintf("ETCD_ADVERTISE_CLIENT_URLS=%s://%s:%s", clientProtocol, *localInstance.PrivateIpAddress, etcdClientPort),
		fmt.Sprintf("ETCD_LISTEN_CLIENT_URLS=%s://0.0.0.0:%s", clientProtocol, etcdClientPort),
		fmt.Sprintf("ETCD_LISTEN_PEER_URLS=%s://0.0.0.0:%s", peerProtocol, etcdPeerPort),
		fmt.Sprintf("ETCD_INITIAL_CLUSTER_STATE=%s", initialClusterState),
		fmt.Sprintf("ETCD_INITIAL_CLUSTER=%s", strings.Join(initialCluster, ",")),
		fmt.Sprintf("ETCD_INITIAL_ADVERTISE_PEER_URLS=%s://%s:%s", peerProtocol, *localInstance.PrivateIpAddress, etcdPeerPort),
		fmt.Sprintf("ETCD_HEARTBEAT_INTERVAL=%d", etcdHeartbeatInterval),
		fmt.Sprintf("ETCD_ELECTION_TIMEOUT=%d", etcdElectionTimeout),
	}
	asg, _ := s.AutoscalingGroup()
	if asg != nil {
		cmd.Env = append(cmd.Env, fmt.Sprintf("ETCD_INITIAL_CLUSTER_TOKEN=%s", *asg.AutoScalingGroupARN))
	}
	for _, env := range cmd.Env {
		log.Printf("%s", env)
	}
	if err := cmd.Run(); err != nil {
		log.Fatalf("%s", err)
	}
}

func buildCluster(s *awsutils.Cluster) (initialClusterState string, initialCluster []string, err error) {
	localInstance, err := s.Instance()
	if err != nil {
		return "", nil, err
	}
	clusterInstances, err := s.Members()
	if err != nil {
		return "", nil, fmt.Errorf("list members: %s", err)
	}
	initialClusterState = "new"
	initialCluster = []string{}
	for _, instance := range clusterInstances {
		if instance.PrivateIpAddress == nil {
			continue
		}
		log.Printf("getting stats from %s (%s)", *instance.InstanceId, *instance.PrivateIpAddress)

		// add this instance to the initialCluster expression
		initialCluster = append(initialCluster, fmt.Sprintf("%s=%s://%s:%s",
			*instance.InstanceId, peerProtocol, *instance.PrivateIpAddress, etcdPeerPort))

		// skip the local node, since we know it is not running yet
		if *instance.InstanceId == *localInstance.InstanceId {
			continue
		}
		path := "stats/self"
		resp, err := getAPIResponse(*instance.PrivateIpAddress, *instance.InstanceId, path, http.MethodGet)
		if err != nil {
			log.Printf("%s: %s://%s:%s/v2/%s: %s", *instance.InstanceId, clientProtocol,
				*instance.PrivateIpAddress, etcdClientPort, path, err)
			continue
		}
		nodeState := etcdState{}
		if err := json.NewDecoder(resp.Body).Decode(&nodeState); err != nil {
			log.Printf("%s: %s://%s:%s/v2/%s: %s", *instance.InstanceId, clientProtocol,
				*instance.PrivateIpAddress, etcdClientPort, path, err)
			continue
		}
		if nodeState.LeaderInfo.Leader == "" {
			log.Printf("%s: %s://%s:%s/v2/%s: alive, no leader", *instance.InstanceId, clientProtocol,
				*instance.PrivateIpAddress, etcdClientPort, path)
			continue
		}
		log.Printf("%s: %s://%s:%s/v2/%s: has leader %s", *instance.InstanceId, clientProtocol,
			*instance.PrivateIpAddress, etcdClientPort, path, nodeState.LeaderInfo.Leader)
		if initialClusterState != "existing" {
			initialClusterState = "existing"
			// inform the node we found about the new node we're about to add so that
			// when etcd starts we can avoid etcd thinking the cluster is out of sync.
			log.Printf("joining cluster via %s", *instance.InstanceId)
			m := etcdMember{
				Name: *localInstance.InstanceId,
				PeerURLs: []string{fmt.Sprintf("%s://%s:%s",
					peerProtocol, *localInstance.PrivateIpAddress, etcdPeerPort)},
			}
			body, _ := json.Marshal(m)
			getAPIResponseWithBody(*instance.PrivateIpAddress, *instance.InstanceId, "members", http.MethodPost, "application/json", bytes.NewReader(body))
		}
	}
	return initialClusterState, initialCluster, nil
}

func getAPIResponse(privateIPAddress string, instanceID string, path string, method string) (*http.Response, error) {
	return getAPIResponseWithBody(privateIPAddress, instanceID, path, method, "", nil)
}

func getAPIResponseWithBody(privateIPAddress string, instanceID string, path string, method string, bodyType string, body io.Reader) (*http.Response, error) {
	var resp *http.Response
	var err error
	var req *http.Request
	if bodyType == "" {
		req, _ = http.NewRequest(method, fmt.Sprintf("%s://%s:%s/v2/%s",
			clientProtocol, privateIPAddress, etcdClientPort, path), body)
	}
	client, err := getHTTPClient()
	if bodyType != "" {
		client.Post(fmt.Sprintf("%s://%s:%s/v2/%s",
			clientProtocol, privateIPAddress, etcdClientPort, path), bodyType, body)
	} else {
		resp, err = client.Do(req)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %s %s://%s:%s/v2/%s: %s",
			instanceID, method, clientProtocol, privateIPAddress, etcdClientPort, path, err)
	}
	return resp, nil
}

func getHTTPClient() (*http.Client, error) {
	var transport *http.Transport
	transport = &http.Transport{}
	client := &http.Client{
		Timeout:   time.Duration(5 * time.Second),
		Transport: transport,
	}
	return client, nil
}

func getEtcdClient(endpoints []string) (*clientv3.Client, error) {
	var etcdClient *clientv3.Client
	var err error

	etcdClient, err = clientv3.New(clientv3.Config{
		Endpoints: endpoints,
	})
	if err != nil {
		return nil, fmt.Errorf("ERROR: %s", err)
	}
	return etcdClient, nil
}
