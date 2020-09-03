/*


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

package framework

import (
	"flag"

	"github.com/pkg/errors"
	"k8s.io/client-go/tools/clientcmd"
)

var GlobalOptions Options

func init() {
	GlobalOptions.BindFlags()
}

type Options struct {
	KubeConfig  string
	ClusterName string
	AWSRegion   string
	AWSVPCID    string
}

func (options *Options) BindFlags() {
	flag.StringVar(&options.KubeConfig, "cluster-kubeconfig", "", "Path to kubeconfig containing embedded authinfo (required)")
	flag.StringVar(&options.ClusterName, "cluster-name", "", `Kubernetes cluster name (required)`)
	flag.StringVar(&options.AWSRegion, "aws-region", "", `AWS Region for the kubernetes cluster`)
	flag.StringVar(&options.AWSVPCID, "aws-vpc-id", "", `AWS VPC ID for the kubernetes cluster`)
}

func (options *Options) Validate() error {
	if len(options.KubeConfig) == 0 {
		return errors.Errorf("%s must be set!", clientcmd.RecommendedConfigPathFlag)
	}
	if len(options.ClusterName) == 0 {
		return errors.Errorf("%s must be set!", "cluster-name")
	}
	if len(options.AWSRegion) == 0 {
		return errors.Errorf("%s must be set!", "aws-region")
	}
	if len(options.AWSVPCID) == 0 {
		return errors.Errorf("%s must be set!", "aws-vpc-id")
	}
	return nil
}
