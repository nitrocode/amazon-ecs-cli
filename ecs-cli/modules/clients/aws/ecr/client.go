// Copyright 2015-2017 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package ecr

import (
	"strings"

	"github.com/aws/amazon-ecs-cli/ecs-cli/modules/clients"
	"github.com/aws/amazon-ecs-cli/ecs-cli/modules/config"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/endpoints"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/aws/aws-sdk-go/service/ecr/ecriface"
	login "github.com/awslabs/amazon-ecr-credential-helper/ecr-login/api"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

const (
	CacheDir = "~/.ecs"
)

// ProcessImageDetails callback function for describe images
type ProcessImageDetails func(images []*ecr.ImageDetail) error

// ProcessRepositories callback function for describe repositories
type ProcessRepositories func(repositories []*string) error

// Client ECR interface
type Client interface {
	GetAuthorizationToken(registryURI string) (*Auth, error)
	GetAuthorizationTokenByID(registryID string) (*Auth, error)
	CreateRepository(repositoryName string) (string, error)
	RepositoryExists(repositoryName string) bool
	GetImages(repositoryNames []*string, tagStatus string, registryID string, processFn ProcessImageDetails) error
}

// ecrClient implements Client
type ecrClient struct {
	client      ecriface.ECRAPI
	loginClient login.Client
	config      *config.CommandConfig
	auth        *Auth
}

// NewClient Creates a new ECR client
func NewClient(config *config.CommandConfig) Client {
	client := ecr.New(config.Session, config.Session.Config)
	client.Handlers.Build.PushBackNamed(clients.CustomUserAgentHandler())
	loginClient := login.DefaultClientFactory{}.NewClientWithOptions(login.Options{
		Session:  config.Session,
		Config:   config.Session.Config,
		CacheDir: CacheDir,
	})
	return newClient(config, client, loginClient)
}

// NewFipsClient Creates a new ECR client that will communicate with a FIPS endpoint.
func NewFipsClient(config *config.CommandConfig) (Client, error) {
	region := aws.StringValue(config.Session.Config.Region)
	resolver := endpoints.DefaultResolver()

	// The convention for FIPS endpoints is to add "-fips" to the official
	// service name; however this would result in an UnknownServiceError
	// being thrown. To allow the endpoint resolver to fallback to the
	// correct endpoint, we can set the ResolveUnknownService flag to true.
	endpoint, err := resolver.EndpointFor("ecr-fips", region, func(opts *endpoints.Options) {
		opts.ResolveUnknownService = true
	})

	if err != nil {
		return nil, err
	}

	log.Debugf("Using FIPS endpoint: %+v", endpoint.URL)

	awsSession := config.Session
	awsConfig := &aws.Config{
		Endpoint: aws.String(endpoint.URL),
	}

	client := ecr.New(awsSession, awsConfig)
	client.Handlers.Build.PushBackNamed(clients.CustomUserAgentHandler())

	// NOTE: While there is a NewClientWithFipsEndpoint method on
	// login.DefaultClientFactory, this would instantiate a login client
	// with a different AWS session/config from that of the ecrClient.
	loginClient := login.DefaultClientFactory{}.NewClientWithOptions(login.Options{
		Session:  awsSession,
		Config:   awsConfig,
		CacheDir: CacheDir,
	})

	return newClient(config, client, loginClient), nil
}

func newClient(config *config.CommandConfig, client ecriface.ECRAPI, loginClient login.Client) Client {
	return &ecrClient{
		config:      config,
		client:      client,
		loginClient: loginClient,
	}
}

// Auth keeps track of the ECR auth
type Auth struct {
	ProxyEndpoint string
	Registry      string
	Username      string
	Password      string
}

func (c *ecrClient) GetAuthorizationTokenByID(registryID string) (*Auth, error) {
	log.Debug("Getting authorization token...")

	auth, err := c.loginClient.GetCredentialsByRegistryID(registryID)
	if err != nil {
		return nil, errors.Wrap(err, "failed to serialize authorization token")
	}
	log.Debugf("Retrieved authorization token via endpoint: %+v", auth.ProxyEndpoint)

	return &Auth{
		Username:      auth.Username,
		Password:      auth.Password,
		ProxyEndpoint: auth.ProxyEndpoint,
		Registry:      strings.Replace(auth.ProxyEndpoint, "https://", "", -1),
	}, nil
}

func (c *ecrClient) GetAuthorizationToken(registryURI string) (*Auth, error) {
	log.Debug("Getting authorization token...")
	auth, err := c.loginClient.GetCredentials(registryURI)
	if err != nil {
		return nil, errors.Wrap(err, "failed to serialize authorization token")
	}
	log.Debugf("Retrieved authorization token via endpoint: %+v", auth.ProxyEndpoint)

	return &Auth{
		Username:      auth.Username,
		Password:      auth.Password,
		ProxyEndpoint: auth.ProxyEndpoint,
		Registry:      strings.Replace(auth.ProxyEndpoint, "https://", "", -1),
	}, nil
}

func (c *ecrClient) RepositoryExists(repositoryName string) bool {
	_, err := c.client.DescribeRepositories(&ecr.DescribeRepositoriesInput{RepositoryNames: []*string{&repositoryName}})
	log.WithFields(log.Fields{
		"repository": repositoryName,
	}).Debug("Check if repository exists")
	return err == nil
}

func (c *ecrClient) CreateRepository(repositoryName string) (string, error) {
	log.WithFields(log.Fields{
		"repository": repositoryName,
	}).Info("Creating repository")

	resp, err := c.client.CreateRepository(
		&ecr.CreateRepositoryInput{RepositoryName: aws.String(repositoryName)})
	if err != nil {
		return "", errors.Wrap(err, "unable to create repository")
	}
	if resp == nil || resp.Repository == nil {
		return "", errors.New("create repository response is empty")
	}

	log.Info("Repository created")
	return aws.StringValue(resp.Repository.RepositoryName), nil
}

func (c *ecrClient) GetImages(repositoryNames []*string, tagStatus string, registryID string, processFn ProcessImageDetails) error {
	log.Debug("Getting images from ECR...")
	pageNumber := 0
	err := c.describeRepositories(repositoryNames, registryID, func(repositories []*string) error {
		for _, repository := range repositories {
			err := c.describeImages(aws.StringValue(repository), tagStatus, registryID, processFn, pageNumber)
			pageNumber++
			if err != nil {
				return err
			}
		}
		return nil
	})

	return err
}

func (c *ecrClient) describeRepositories(repositoryNames []*string, registryID string, outputFn ProcessRepositories) error {
	var outErr error

	input := &ecr.DescribeRepositoriesInput{}

	// Skip DescribeRepositories calls if repositoryNames are specified
	if len(repositoryNames) > 0 {
		err := outputFn(repositoryNames)
		return err
	}

	if registryID != "" {
		input.SetRegistryId(registryID)
	}

	err := c.client.DescribeRepositoriesPages(input, func(resp *ecr.DescribeRepositoriesOutput, lastPage bool) bool {
		repositoryNames = []*string{}
		for _, repository := range resp.Repositories {
			repositoryNames = append(repositoryNames, repository.RepositoryName)
		}
		if outErr = outputFn(repositoryNames); outErr != nil {
			return false
		}
		return !lastPage
	})

	if err != nil {
		return err
	}
	return outErr
}

func (c *ecrClient) describeImages(repositoryName string, tagStatus string, registryID string, outputFn ProcessImageDetails, numOfCalls int) error {
	var outErr error

	filter := &ecr.DescribeImagesFilter{}
	if tagStatus != "" {
		filter.SetTagStatus(tagStatus)
	}

	input := &ecr.DescribeImagesInput{
		RepositoryName: aws.String(repositoryName),
		Filter:         filter,
	}

	if registryID != "" {
		input.SetRegistryId(registryID)
	}

	err := c.client.DescribeImagesPages(input, func(resp *ecr.DescribeImagesOutput, lastPage bool) bool {
		if outErr = outputFn(resp.ImageDetails); outErr != nil {
			return false
		}
		if numOfCalls > 50 {
			outErr = errors.New("please specify the repository name if you wish to see more")
			return false
		}
		return !lastPage
	})

	if err != nil {
		return err
	}
	return outErr
}
