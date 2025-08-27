package main

import (
	"context"
	"strings"

	"github.com/kubeshop/botkube/pkg/api"
)

// Help returns interactive help message with common examples.
func (e *Executor) Help(context.Context) (api.Message, error) {
	btn := api.NewMessageButtonBuilder()

	identity := []api.Button{
		btn.ForCommandWithDescCmd("Who am I?", "aws sts get-caller-identity"),
		btn.ForCommandWithDescCmd("Version", "aws --version"),
	}
	compute := []api.Button{
		btn.ForCommandWithDescCmd("EC2 instances", "aws ec2 describe-instances"),
		btn.ForCommandWithDescCmd("EKS clusters", "aws eks list-clusters"),
		btn.ForCommandWithDescCmd("ECS clusters", "aws ecs list-clusters"),
		btn.ForCommandWithDescCmd("Lambda functions", "aws lambda list-functions"),
	}
	storage := []api.Button{
		btn.ForCommandWithDescCmd("S3 buckets", "aws s3api list-buckets"),
	}
	database := []api.Button{
		btn.ForCommandWithDescCmd("RDS instances", "aws rds describe-db-instances"),
		btn.ForCommandWithDescCmd("DynamoDB tables", "aws dynamodb list-tables"),
		btn.ForCommandWithDescCmd("ElastiCache clusters", "aws elasticache describe-cache-clusters"),
	}
	network := []api.Button{
		btn.ForCommandWithDescCmd("VPCs", "aws ec2 describe-vpcs"),
		btn.ForCommandWithDescCmd("Subnets", "aws ec2 describe-subnets"),
	}
	updates := []api.Button{
		btn.ForCommandWithDescCmd("EC2 RebootInstances (picker)", "aws ec2 reboot-instances --instance-ids <i-xxxxxxxxxxxxxxxxx>"),
	}

	return api.Message{
		OnlyVisibleForYou: true,
		Sections: []api.Section{
			{
				Base: api.Base{
					Header:      "Run AWS CLI",
					Description: "Examples: `aws --version`, `aws sts get-caller-identity`, `aws ec2 describe-instances --max-results 5`",
				},
				Buttons: identity,
			},
			{Base: api.Base{Header: "Compute (examples)"}, Buttons: compute},
			{Base: api.Base{Header: "Storage (examples)"}, Buttons: storage},
			{Base: api.Base{Header: "Database (examples)"}, Buttons: database},
			{Base: api.Base{Header: "Networking (examples)"}, Buttons: network},
			{
				Base: api.Base{
					Header:      "Limited Update operations",
					Description: "Operations may be restricted by policy.",
				},
				Buttons: updates,
			},
		},
	}, nil
}

// fullHelpText returns a long, example-rich help as a code block.
func fullHelpText() string {
	return strings.TrimSpace(`Run AWS CLI
ex) aws --version, aws sts get-caller-identity, aws ec2 describe-instances --max-results 5

@black aws sts get-caller-identity
@black aws --version

Compute
@black aws ec2 describe-instances
@black aws eks list-clusters
@black aws ecs list-clusters
@black aws lambda list-functions

Storage
@black aws s3api list-buckets

Database
@black aws rds describe-db-instances
@black aws dynamodb list-tables
@black aws elasticache describe-cache-clusters

Networking
@black aws ec2 describe-vpcs
@black aws ec2 describe-subnets

Limited Update operations
@black aws ec2 reboot-instances --instance-ids <i-xxxxxxxxxxxxxxxxx>`)
}
