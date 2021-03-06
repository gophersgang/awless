/*
Copyright 2017 WALLIX

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
	"fmt"
	"regexp"
	"sort"
	"sync"

	awssdk "github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/endpoints"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/aws/aws-sdk-go/service/sts/stsiface"
	"github.com/wallix/awless/graph"
)

var DefaultAMIUsers = []string{"ec2-user", "ubuntu", "centos", "bitnami", "admin", "root"}

func AllRegions() []string {
	var regions sort.StringSlice
	partitions := endpoints.DefaultResolver().(endpoints.EnumPartitions).Partitions()
	for _, p := range partitions {
		for id := range p.Regions() {
			regions = append(regions, id)
		}
	}
	sort.Sort(regions)
	return regions
}

func IsValidRegion(given string) bool {
	reg, _ := regexp.Compile("^(us|eu|ap|sa|ca)\\-\\w+\\-\\d+$")
	regChina, _ := regexp.Compile("^cn\\-\\w+\\-\\d+$")
	regUsGov, _ := regexp.Compile("^us\\-gov\\-\\w+\\-\\d+$")

	return reg.MatchString(given) || regChina.MatchString(given) || regUsGov.MatchString(given)
}

type Security interface {
	stsiface.STSAPI
	GetUserId() (string, error)
	GetAccountId() (string, error)
}

type oncer struct {
	sync.Once
	result interface{}
	err    error
}

type security struct {
	stsiface.STSAPI
}

func NewSecu(sess *session.Session) Security {
	return &security{sts.New(sess)}
}

func (s *security) GetUserId() (string, error) {
	output, err := s.GetCallerIdentity(&sts.GetCallerIdentityInput{})
	if err != nil {
		return "", err
	}
	return awssdk.StringValue(output.Arn), nil
}

func (s *security) GetAccountId() (string, error) {
	output, err := s.GetCallerIdentity(&sts.GetCallerIdentityInput{})
	if err != nil {
		return "", err
	}
	return awssdk.StringValue(output.Account), nil
}

func (s *Access) fetch_all_user_graph() (*graph.Graph, []*iam.UserDetail, error) {
	g := graph.NewGraph()
	var userDetails []*iam.UserDetail

	var wg sync.WaitGroup
	errc := make(chan error)

	wg.Add(1)
	go func() {
		defer wg.Done()

		out, err := s.GetAccountAuthorizationDetails(&iam.GetAccountAuthorizationDetailsInput{
			Filter: []*string{
				awssdk.String(iam.EntityTypeUser),
			},
		})
		if err != nil {
			errc <- err
			return
		}

		for _, output := range out.UserDetailList {
			userDetails = append(userDetails, output)
			res, err := newResource(output)
			if err != nil {
				errc <- err
				return
			}
			g.AddResource(res)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()

		out, err := s.ListUsers(&iam.ListUsersInput{})
		if err != nil {
			errc <- err
			return
		}

		for _, output := range out.Users {
			res, err := newResource(output)
			if err != nil {
				errc <- err
				return
			}
			g.AddResource(res)
		}
	}()

	go func() {
		wg.Wait()
		close(errc)
	}()

	for err := range errc {
		if err != nil {
			return g, userDetails, err
		}
	}

	return g, userDetails, nil
}

func (s *Storage) fetch_all_bucket_graph() (*graph.Graph, []*s3.Bucket, error) {
	g := graph.NewGraph()
	var buckets []*s3.Bucket
	bucketM := &sync.Mutex{}

	err := s.foreach_bucket_parallel(func(b *s3.Bucket) error {
		bucketM.Lock()
		buckets = append(buckets, b)
		bucketM.Unlock()
		res, err := newResource(b)
		g.AddResource(res)
		if err != nil {
			return fmt.Errorf("build resource for bucket `%s`: %s", awssdk.StringValue(b.Name), err)
		}
		return nil
	})
	return g, buckets, err
}

func (s *Storage) fetch_all_storageobject_graph() (*graph.Graph, []*s3.Object, error) {
	g := graph.NewGraph()
	var cloudResources []*s3.Object

	err := s.foreach_bucket_parallel(func(b *s3.Bucket) error {
		return s.fetchObjectsForBucket(b, g)
	})

	return g, cloudResources, err
}

func (s *Storage) fetchObjectsForBucket(bucket *s3.Bucket, g *graph.Graph) error {
	out, err := s.ListObjects(&s3.ListObjectsInput{Bucket: bucket.Name})
	if err != nil {
		return err
	}

	for _, output := range out.Contents {
		res, err := newResource(output)
		if err != nil {
			return err
		}
		res.Properties["BucketName"] = awssdk.StringValue(bucket.Name)
		g.AddResource(res)
		parent, err := initResource(bucket)
		if err != nil {
			return err
		}
		g.AddParentRelation(parent, res)
	}

	return nil
}

func (s *Storage) getBucketsPerRegion() ([]*s3.Bucket, error) {
	var buckets []*s3.Bucket
	out, err := s.ListBuckets(&s3.ListBucketsInput{})
	if err != nil {
		return buckets, err
	}

	bucketc := make(chan *s3.Bucket)
	errc := make(chan error)

	var wg sync.WaitGroup

	for _, bucket := range out.Buckets {
		wg.Add(1)
		go func(b *s3.Bucket) {
			defer wg.Done()
			loc, err := s.GetBucketLocation(&s3.GetBucketLocationInput{Bucket: b.Name})
			if err != nil {
				errc <- err
				return
			}
			switch awssdk.StringValue(loc.LocationConstraint) {
			case "":
				if s.region == "us-east-1" {
					bucketc <- b
				}
			case s.region:
				bucketc <- b
			}
		}(bucket)
	}
	go func() {
		wg.Wait()
		close(bucketc)
	}()

	for {
		select {
		case err := <-errc:
			if err != nil {
				return buckets, err
			}
		case b, ok := <-bucketc:
			if !ok {
				return buckets, nil
			}
			buckets = append(buckets, b)
		}
	}
}

func (s *Storage) foreach_bucket_parallel(f func(b *s3.Bucket) error) error {
	s.once.Do(func() {
		s.once.result, s.once.err = s.getBucketsPerRegion()
	})
	if s.once.err != nil {
		return s.once.err
	}
	buckets := s.once.result.([]*s3.Bucket)

	errc := make(chan error)
	var wg sync.WaitGroup

	for _, output := range buckets {
		wg.Add(1)
		go func(b *s3.Bucket) {
			defer wg.Done()
			if err := f(b); err != nil {
				errc <- err
			}
		}(output)
	}
	go func() {
		wg.Wait()
		close(errc)
	}()

	for err := range errc {
		if err != nil {
			return err
		}
	}

	return nil
}
