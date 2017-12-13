package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
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
	"github.com/coreos/etcd/pkg/transport"
)

// Service :
type Service struct {
	instance         string
	clusterTagName   string
	etcdMajorVersion string
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
	etcdMajorVersion      *int64
	etcdPeerPort          *int64
	peerProtocol          string
	clientProtocol        string
	etcdClientPort        *int64
	etcdCertFile          *string
	etcdKeyFile           *string
	etcdTrustedCaFile     *string
	etcdPeerCertFile      *string
	etcdPeerKeyFile       *string
	etcdPeerTrustedCaFile *string
	etcdHeartbeatInterval *int64
	etcdElectionTimeout   *int64
	clientTLSEnabled      bool
)

func main() {
	svc := new(Service)
	flag.StringVar(&svc.instance, "instance", "", "The instance ID of the cluster member. If not supplied, then the instance ID is determined from EC2 metadata")
	flag.StringVar(&svc.clusterTagName, "clusterTagName", "aws:autoscaling:groupName", "The instance tag that is common to all members of the cluster")

	dataDir := flag.String("data-dir", "/var/lib/etcd",
		"The path to the etcd data directory. "+
			"Environment variable: ETCD_DATA_DIR")

	lifecycleQueueName := flag.String("lifecycle-queue-name", "",
		"The name of the lifecycle SQS queue (optional). "+
			"Environment variable: LIFECYCLE_QUEUE_NAME")

	etcdMajorVersion = flag.Int64("etcd-major-version", 3,
		"Etcd API version (2, 3). "+
			"Environment variable: ETCD_MAJOR_VERSION")

	etcdClientPort = flag.Int64("etcd-client-port", 2379,
		"Etcd client port number. "+
			"Environment variable: ETCD_CLIENT_PORT")

	etcdPeerPort = flag.Int64("etcd-peer-port", 2380,
		"Etcd peer port number. "+
			"Environment variable: ETCD_PEER_PORT")

	etcdCertFile = flag.String("etcd-cert-file", os.Getenv("ETCD_CERT_FILE"),
		"Path to the client server TLS cert file. "+
			"Environment variable: ETCD_CERT_FILE")

	etcdKeyFile = flag.String("etcd-key-file", os.Getenv("ETCD_KEY_FILE"),
		"Path to the client server TLS key file. "+
			"Environment variable: ETCD_KEY_FILE")

	etcdClientCertAuth := flag.Bool("etcd-client-cert-auth", false,
		"Enable client cert authentication. "+
			"Environment variable: ETCD_CLIENT_CERT_AUTH")

	etcdTrustedCaFile = flag.String("etcd-trusted-ca-file", os.Getenv("ETCD_TRUSTED_CA_FILE"),
		"Path to the client server TLS trusted CA key file. "+
			"Environment variable: ETCD_TRUSTED_CA_FILE")

	etcdPeerCertFile := flag.String("etcd-peer-cert-file", os.Getenv("ETCD_PEER_CERT_FILE"),
		"Path to the peer server TLS cert file. "+
			"Environment variable: ETCD_PEER_CERT_FILE")

	etcdPeerKeyFile := flag.String("etcd-peer-key-file", os.Getenv("ETCD_PEER_KEY_FILE"),
		"Path to the peer server TLS key file. "+
			"Environment variable: ETCD_PEER_KEY_FILE")

	etcdPeerClientCertAuth := flag.Bool("etcd-peer-client-cert-auth", false,
		"Enable peer client cert authentication. "+
			"Environment variable: ETCD_PEER_CLIENT_CERT_AUTH")

	etcdPeerTrustedCaFile := flag.String("etcd-peer-trusted-ca-file", os.Getenv("ETCD_PEER_TRUSTED_CA_FILE"),
		"Path to the peer client server TLS trusted CA key file. "+
			"Environment variable: ETCD_PEER_TRUSTED_CA_FILE")

	etcdHeartbeatInterval = flag.Int64("etcd-heartbeat-interval", 100,
		"Time (in milliseconds) of a heartbeat interval. "+
			"Environment variable: ETCD_HEARTBEAT_INTERVAL")

	etcdElectionTimeout = flag.Int64("etcd-election-timeout", 500,
		"Time (in milliseconds) for an election to timeout. "+
			"Environment variable: ETCD_ELECTION_TIMEOUT")

	flag.Parse()

	clientTLSEnabled = false
	clientProtocol = "http"
	if *etcdCertFile != "" {
		clientTLSEnabled = true
		*etcdClientCertAuth = true
		*etcdPeerClientCertAuth = true
		clientProtocol = "https"
	}
	peerProtocol = "http"
	if *etcdPeerCertFile != "" {
		peerProtocol = "https"
	}

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
			etcdClient, err = getEtcdClient([]string{fmt.Sprintf("%s://%s:%d",
				clientProtocol, *localInstance.PrivateIpAddress, *etcdClientPort)})
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

	cmd := exec.Command(fmt.Sprintf("etcd%s", svc.etcdMajorVersion))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = []string{
		fmt.Sprintf("ETCD_NAME=%s", *localInstance.InstanceId),
		fmt.Sprintf("ETCD_DATA_DIR=%s", "/var/lib/etcd"),
		fmt.Sprintf("ETCD_ADVERTISE_CLIENT_URLS=%s://%s:%d", clientProtocol, *localInstance.PrivateIpAddress, *etcdClientPort),
		fmt.Sprintf("ETCD_DATA_DIR=%s", *dataDir),
		fmt.Sprintf("ETCD_LISTEN_CLIENT_URLS=%s://0.0.0.0:%d", clientProtocol, *etcdClientPort),
		fmt.Sprintf("ETCD_LISTEN_PEER_URLS=%s://0.0.0.0:%d", peerProtocol, *etcdPeerPort),
		fmt.Sprintf("ETCD_INITIAL_CLUSTER_STATE=%s", initialClusterState),
		fmt.Sprintf("ETCD_INITIAL_CLUSTER=%s", strings.Join(initialCluster, ",")),
		fmt.Sprintf("ETCD_INITIAL_ADVERTISE_PEER_URLS=%s://%s:%d", peerProtocol, *localInstance.PrivateIpAddress, *etcdPeerPort),
		fmt.Sprintf("ETCD_HEARTBEAT_INTERVAL=%d", *etcdHeartbeatInterval),
		fmt.Sprintf("ETCD_ELECTION_TIMEOUT=%d", *etcdElectionTimeout),
		fmt.Sprintf("ETCD_CERT_FILE=%s", *etcdCertFile),
		fmt.Sprintf("ETCD_KEY_FILE=%s", *etcdKeyFile),
		fmt.Sprintf("ETCD_CLIENT_CERT_AUTH=%v", *etcdClientCertAuth),
		fmt.Sprintf("ETCD_TRUSTED_CA_FILE=%s", *etcdTrustedCaFile),
		fmt.Sprintf("ETCD_PEER_CERT_FILE=%s", *etcdPeerCertFile),
		fmt.Sprintf("ETCD_PEER_KEY_FILE=%s", *etcdPeerKeyFile),
		fmt.Sprintf("ETCD_PEER_CLIENT_CERT_AUTH=%v", *etcdPeerClientCertAuth),
		fmt.Sprintf("ETCD_PEER_TRUSTED_CA_FILE=%s", *etcdPeerTrustedCaFile),
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
		initialCluster = append(initialCluster, fmt.Sprintf("%s=%s://%s:%d",
			*instance.InstanceId, peerProtocol, *instance.PrivateIpAddress, *etcdPeerPort))

		// skip the local node, since we know it is not running yet
		if *instance.InstanceId == *localInstance.InstanceId {
			continue
		}
		path := "stats/self"
		resp, err := getAPIResponse(*instance.PrivateIpAddress, *instance.InstanceId, path, http.MethodGet)
		if err != nil {
			log.Printf("%s: %s://%s:%d/v2/%s: %s", *instance.InstanceId, clientProtocol,
				*instance.PrivateIpAddress, *etcdClientPort, path, err)
			continue
		}
		nodeState := etcdState{}
		if err := json.NewDecoder(resp.Body).Decode(&nodeState); err != nil {
			log.Printf("%s: %s://%s:%d/v2/%s: %s", *instance.InstanceId, clientProtocol,
				*instance.PrivateIpAddress, *etcdClientPort, path, err)
			continue
		}
		if nodeState.LeaderInfo.Leader == "" {
			log.Printf("%s: %s://%s:%d/v2/%s: alive, no leader", *instance.InstanceId, clientProtocol,
				*instance.PrivateIpAddress, *etcdClientPort, path)
			continue
		}
		log.Printf("%s: %s://%s:%d/v2/%s: has leader %s", *instance.InstanceId, clientProtocol,
			*instance.PrivateIpAddress, *etcdClientPort, path, nodeState.LeaderInfo.Leader)
		if initialClusterState != "existing" {
			initialClusterState = "existing"
			// inform the node we found about the new node we're about to add so that
			// when etcd starts we can avoid etcd thinking the cluster is out of sync.
			log.Printf("joining cluster via %s", *instance.InstanceId)
			m := etcdMember{
				Name: *localInstance.InstanceId,
				PeerURLs: []string{fmt.Sprintf("%s://%s:%d",
					peerProtocol, *localInstance.PrivateIpAddress, *etcdPeerPort)},
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
	var apiVersion string
	var req *http.Request
	if path != "health" {
		apiVersion = "/v2"
	}
	req, err = http.NewRequest(method, fmt.Sprintf("%s://%s:%d%s/%s",
		clientProtocol, privateIPAddress, *etcdClientPort, apiVersion, path), body)

	if err != nil {
		return resp, fmt.Errorf("%s: %s %s: %s",
			instanceID, method, req.URL, err)
	}
	if method != "GET" && method != "DELETE" && bodyType != "" {
		req.Header.Add("Content-Type", bodyType)
	}
	client, err := getHTTPClient()
	if err != nil {
		return resp, fmt.Errorf("%s: %s %s: %s",
			instanceID, method, req.URL, err)
	}
	resp, err = client.Do(req)

	if err != nil {
		return resp, fmt.Errorf("%s: %s %s: %s",
			instanceID, method, req.URL, err)
	}
	return resp, nil
}

func getHTTPClient() (*http.Client, error) {
	var transport *http.Transport
	if clientTLSEnabled {
		cert, err := tls.LoadX509KeyPair(*etcdCertFile, *etcdKeyFile)
		if err != nil {
			return nil, fmt.Errorf("ERROR: %s", err)
		}
		caCert, err := ioutil.ReadFile(*etcdTrustedCaFile)
		if err != nil {
			return nil, fmt.Errorf("ERROR: %s", err)
		}
		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(caCert)
		tlsConfig := &tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      caCertPool,
		}
		tlsConfig.BuildNameToCertificate()
		transport = &http.Transport{
			TLSClientConfig:     tlsConfig,
			TLSHandshakeTimeout: 2 * time.Second,
		}
	} else {
		transport = &http.Transport{}
	}
	transport.IdleConnTimeout = 10 * time.Second
	transport.ResponseHeaderTimeout = 2 * time.Second
	transport.ExpectContinueTimeout = 10 * time.Second
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second}
	return client, nil
}

func getEtcdClient(endpoints []string) (*clientv3.Client, error) {
	var etcdClient *clientv3.Client
	var err error
	if clientTLSEnabled {
		tlsInfo := transport.TLSInfo{
			CertFile:      *etcdCertFile,
			KeyFile:       *etcdKeyFile,
			TrustedCAFile: *etcdTrustedCaFile,
		}
		tlsConfig, err := tlsInfo.ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("ERROR: %s", err)
		}
		etcdClient, err = clientv3.New(clientv3.Config{
			Endpoints: endpoints,
			TLS:       tlsConfig,
		})
		if err != nil {
			return nil, fmt.Errorf("ERROR: %s", err)
		}
	} else {
		etcdClient, err = clientv3.New(clientv3.Config{
			Endpoints: endpoints,
		})
		if err != nil {
			return nil, fmt.Errorf("ERROR: %s", err)
		}
	}
	return etcdClient, nil
}
