package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/ec2"
)

// Cluster :
type Cluster struct {
	AwsSession *session.Session
	InstanceID string
	TagName    string
	TagValue   string

	instance         *ec2.Instance
	autoScalingGroup *autoscaling.Group
	members          []*ec2.Instance
}

// Instance :
func (s *Cluster) Instance() (*ec2.Instance, error) {
	if s.instance != nil {
		return s.instance, nil
	}
	ec2svc := ec2.New(s.AwsSession)
	resp, err := ec2svc.DescribeInstances(&ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String(s.InstanceID)},
	})
	if err != nil {
		log.Println(err.Error())
		return nil, err
	}
	if len(resp.Reservations) != 1 || len(resp.Reservations[0].Instances) != 1 {
		return nil, fmt.Errorf("Cannot find instance %s", s.InstanceID)
	}
	s.instance = resp.Reservations[0].Instances[0]
	return s.instance, nil
}

type byLaunchTime []*ec2.Instance

func (a byLaunchTime) Len() int           { return len(a) }
func (a byLaunchTime) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a byLaunchTime) Less(i, j int) bool { return a[i].LaunchTime.Before(*a[j].LaunchTime) }

// Members :
func (s *Cluster) Members() ([]*ec2.Instance, error) {
	tagValue := s.TagValue
	if tagValue == "" {
		instance, err := s.Instance()
		if err != nil {
			return nil, err
		}
		for _, tag := range instance.Tags {
			if *tag.Key == s.TagName {
				tagValue = *tag.Value
			}
		}
	}
	if tagValue == "" {
		return nil, fmt.Errorf("Current instance (%s) does not have a tag %s",
			s.InstanceID, s.TagName)
	}
	ec2svc := ec2.New(s.AwsSession)
	members := []*ec2.Instance{}
	err := ec2svc.DescribeInstancesPages(&ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{
			&ec2.Filter{
				Name:   aws.String(fmt.Sprintf("tag:%s", s.TagName)),
				Values: []*string{aws.String(tagValue)},
			},
		},
	}, func(resp *ec2.DescribeInstancesOutput, lastPage bool) (shouldContinue bool) {
		for _, reservation := range resp.Reservations {
			for _, instance := range reservation.Instances {
				members = append(members, instance)
			}
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	sort.Sort(byLaunchTime(members))
	s.members = members
	return s.members, nil
}

// AutoscalingGroup :
func (s *Cluster) AutoscalingGroup() (*autoscaling.Group, error) {
	instance, err := s.Instance()
	if err != nil {
		return nil, err
	}
	if s.autoScalingGroup != nil {
		return s.autoScalingGroup, nil
	}
	autoscalingGroupName := ""
	for _, tag := range instance.Tags {
		if *tag.Key == "aws:autoscaling:groupName" {
			autoscalingGroupName = *tag.Value
		}
	}
	if autoscalingGroupName == "" {
		return nil, nil
	}
	autoscalingService := autoscaling.New(s.AwsSession)
	groupInfo, err := autoscalingService.DescribeAutoScalingGroups(&autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []*string{aws.String(autoscalingGroupName)},
		MaxRecords:            aws.Int64(1),
	})
	if err != nil {
		return nil, err
	}
	if len(groupInfo.AutoScalingGroups) != 1 {
		return nil, fmt.Errorf("cannot find autoscaling group %s", autoscalingGroupName)
	}
	s.autoScalingGroup = groupInfo.AutoScalingGroups[0]
	return s.autoScalingGroup, nil
}

// DiscoverInstanceID :
func DiscoverInstanceID() (string, error) {
	return readMetadata("instance-id")
}

func readMetadata(suffix string) (string, error) {
	// a nice short timeout so we don't hang too much on non-AWS boxes
	client := *http.DefaultClient
	client.Timeout = 700 * time.Millisecond

	resp, err := client.Get("http://169.254.169.254/latest/meta-data/" + suffix)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetching metadata: %s", resp.Status)
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}
