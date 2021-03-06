/*
Copyright 2018 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package aws

import (
	"context"
	"fmt"

	gaws "github.com/gravitational/gravity/lib/cloudprovider/aws"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/sqs"
	"github.com/aws/aws-sdk-go/service/ssm"

	"github.com/gravitational/trace"
	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
)

const (
	// InstanceLaunching is AWS instance launching lifecycle autoscaling event
	InstanceLaunching = "autoscaling:EC2_INSTANCE_LAUNCHING"
	// InstanceTermination is AWS instance terminating lifecycle autoscaling event
	InstanceTerminating = "autoscaling:EC2_INSTANCE_TERMINATING"
	// sourceDestCheck is AWS source destination check property
	sourceDestCheck = "sourceDestCheck"
	// instanceIDFilter is AWS instance ID filter
	instanceIDFilter = "instance-id"
)

// Autoscaler is AWS autoscaler server, it enables nodes
// to discover cluster information via AWS Systems Manager (SSM) Parameter Store
// and Masters to add/remove nodes from the cluster as they join
// via discovery group
type Autoscaler struct {
	// Config is Autoscaler config
	Config
	// QueueURL is SQS queue name with notifications
	QueueURL string
	*log.Entry
}

// Config is autoscaler config
type Config struct {
	// ClusterName is a Telekube cluster name,
	// used to discover configuration in the cluster
	ClusterName string
	// Client is an optional kubernetes client
	Client *kubernetes.Clientset
	// SSM is AWS systems manager parameter store,
	// metadata store used to store configuration
	SystemsManager SSM
	// Queue is Simple Queue Service, AWS pub/sub queue
	Queue SQS
	// Cloud is Elastic Compute Cloud, AWS cloud service
	Cloud EC2
	// NewLocalInstance is used to retrieve local instance metadata
	NewLocalInstance NewLocalInstance
}

// CheckAndSetDefaults checks and sets default values
func (cfg *Config) CheckAndSetDefaults() error {
	if cfg.ClusterName == "" {
		return trace.BadParameter("missing parameter ClusterName")
	}
	if cfg.NewLocalInstance == nil {
		cfg.NewLocalInstance = gaws.NewLocalInstance
	}
	return nil
}

// New returns new instance of AWS autoscaler
func New(cfg Config) (*Autoscaler, error) {
	if err := cfg.CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}
	instance, err := cfg.NewLocalInstance()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	sess, err := session.NewSession(&aws.Config{
		Region: &instance.Region,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if cfg.SystemsManager == nil {
		cfg.SystemsManager = ssm.New(sess)
	}
	if cfg.Queue == nil {
		cfg.Queue = sqs.New(sess)
	}
	if cfg.Cloud == nil {
		cfg.Cloud = ec2.New(sess)
	}
	a := &Autoscaler{
		Config: cfg,
		Entry:  log.WithFields(log.Fields{trace.Component: "autoscale"}),
	}
	return a, nil
}

// DeleteEvent deletes SQS message associated with event
func (a *Autoscaler) DeleteEvent(ctx context.Context, event HookEvent) error {
	a.Debugf("DeleteEvent(%v)", event.Type)
	_, err := a.Queue.DeleteMessageWithContext(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(event.QueueURL),
		ReceiptHandle: aws.String(event.ReceiptHandle),
	})
	return trace.Wrap(err)
}

// TurnOffSourceDestination check turns off source destination check on the instance
// that is necessary for K8s to function properly
func (a *Autoscaler) TurnOffSourceDestinationCheck(ctx context.Context, instanceID string) error {
	a.Debugf("TurnOffSourceDestinationCheck(%v)", instanceID)
	_, err := a.Cloud.ModifyInstanceAttributeWithContext(ctx, &ec2.ModifyInstanceAttributeInput{
		InstanceId:      aws.String(instanceID),
		SourceDestCheck: &ec2.AttributeBooleanValue{Value: aws.Bool(false)},
	})
	return trace.Wrap(err)
}

// GetJoinToken fetches and decrypts cluster join token from SSM parameter
func (a *Autoscaler) GetJoinToken(ctx context.Context) (string, error) {
	name := a.tokenParam()
	a.Debugf("GetJoinToken(%v)", name)
	resp, err := a.SystemsManager.GetParameterWithContext(ctx, &ssm.GetParameterInput{
		Name:           aws.String(name),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		return "", ConvertError(err)
	}
	return aws.StringValue(resp.Parameter.Value), nil
}

// GetServiceURL returns service URL
func (a *Autoscaler) GetServiceURL(ctx context.Context) (string, error) {
	name := a.serviceURLParam()
	a.Debugf("GetServiceURL(%v)", name)
	resp, err := a.SystemsManager.GetParameterWithContext(ctx, &ssm.GetParameterInput{
		Name:           aws.String(name),
		WithDecryption: aws.Bool(false),
	})
	if err != nil {
		return "", ConvertError(err)
	}
	return aws.StringValue(resp.Parameter.Value), nil
}

func (a *Autoscaler) publishServiceURL(ctx context.Context, serviceURL string) error {
	name := a.serviceURLParam()
	_, err := a.SystemsManager.PutParameterWithContext(ctx, &ssm.PutParameterInput{
		Type:      aws.String("String"),
		Name:      aws.String(name),
		Value:     aws.String(serviceURL),
		Overwrite: aws.Bool(true),
	})
	if err != nil {
		return ConvertError(err)
	}
	return nil
}

func (a *Autoscaler) publishJoinToken(ctx context.Context, token string) error {
	name := a.tokenParam()
	a.Debugf("PublishJoinToken(%v)", name)
	_, err := a.SystemsManager.PutParameterWithContext(ctx, &ssm.PutParameterInput{
		Type:      aws.String("SecureString"),
		Name:      aws.String(name),
		Value:     aws.String(token),
		Overwrite: aws.Bool(true),
	})
	if err != nil {
		return ConvertError(err)
	}
	return nil
}

func (a *Autoscaler) tokenParam() string {
	return fmt.Sprintf("/telekube/%v/token", a.ClusterName)
}

func (a *Autoscaler) serviceURLParam() string {
	return fmt.Sprintf("/telekube/%v/service", a.ClusterName)
}

// ConvertError converts errors specific to AWS to trace-compatible error
func ConvertError(err error, args ...interface{}) error {
	if err == nil {
		return nil
	}
	if awsErr, ok := err.(awserr.Error); ok {
		switch awsErr.Code() {
		case ssm.ErrCodeParameterAlreadyExists:
			return trace.AlreadyExists(awsErr.Error(), args...)
		case ssm.ErrCodeParameterNotFound, ssm.ErrCodeParameterVersionNotFound:
			return trace.NotFound(awsErr.Error(), args...)
		default:
			return trace.BadParameter(awsErr.Error(), args...)
		}
	}
	return err
}
