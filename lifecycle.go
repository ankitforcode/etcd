package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/ankitforcode/awsutils"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/sqs"
)

func handleLifecycleEvent(m *awsutils.LifecycleMessage) (shouldContinue bool, err error) {
	if m.LifecycleTransition != "autoscaling:EC2_INSTANCE_TERMINATING" {
		return true, nil
	}
	resp, err := getAPIResponse(*localInstance.PrivateIpAddress, *localInstance.InstanceId, "members", http.MethodGet)
	if err != nil {
		return false, err
	}
	members := etcdMembers{}
	if err := json.NewDecoder(resp.Body).Decode(&members); err != nil {
		return false, err
	}
	memberID := ""
	for _, member := range members.Members {
		if member.Name == m.EC2InstanceID {
			memberID = member.ID
		}
	}
	if memberID == "" {
		log.WithField("InstanceID", m.EC2InstanceID).Warn("received termination event for non-member")
		return true, nil
	}

	log.WithFields(log.Fields{
		"InstanceID": m.EC2InstanceID,
		"MemberID":   memberID}).Info("removing from cluster")

	resp, err = getAPIResponse(*localInstance.PrivateIpAddress, *localInstance.InstanceId, fmt.Sprintf("members/%s", memberID), http.MethodDelete)
	if err != nil {
		return false, err
	}

	return false, nil
}

func watchLifecycleEvents(s *awsutils.Cluster, queueName string) {
	localInstance, _ = s.Instance()
	for {
		q, err := LifecycleEventQueueURL(s, queueName)
		if err != nil {
			log.Fatalf("ERROR: LifecycleEventQueueURL: %s", err)
		}
		log.Printf("SQS queue URL: %s", q)
		err = s.WatchLifecycleEvents(q, handleLifecycleEvent)
		if err == awsutils.ErrLifecycleHookNotFound {
			log.Printf("WARNING: %s", err)
			time.Sleep(10 * time.Second)
			continue
		}
		if err != nil {
			log.Fatalf("ERROR: WatchLifecycleEvents: %s", err)
		}
		panic("not reached")
	}
}

// LifecycleEventQueueURL :
func LifecycleEventQueueURL(s *awsutils.Cluster, queueName string) (string, error) {
	asg, err := s.AutoscalingGroup()
	if err != nil {
		return "", err
	}
	autoscalingSvc := autoscaling.New(s.AwsSession)
	resp, err := autoscalingSvc.DescribeLifecycleHooks(&autoscaling.DescribeLifecycleHooksInput{
		AutoScalingGroupName: asg.AutoScalingGroupName,
	})
	if err != nil {
		return "", err
	}
	sqsSvc := sqs.New(s.AwsSession)
	for _, hook := range resp.LifecycleHooks {
		if !strings.HasPrefix(*hook.NotificationTargetARN, "arn:aws:sqs:") {
			continue
		}
		arnParts := strings.Split(*hook.NotificationTargetARN, ":")
		qName := arnParts[len(arnParts)-1]
		qOwnerAWSAccountID := arnParts[len(arnParts)-2]

		if queueName != "" && !strings.Contains(qName, "-"+queueName+"-") {
			continue
		}

		resp, err := sqsSvc.GetQueueUrl(&sqs.GetQueueUrlInput{
			QueueName:              &qName,
			QueueOwnerAWSAccountId: &qOwnerAWSAccountID,
		})
		if err != nil {
			return "", err
		}
		return *resp.QueueUrl, nil
	}
	return "", awsutils.ErrLifecycleHookNotFound
}
