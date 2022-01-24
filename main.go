package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"

	"github.com/pulumi/pulumi-aws/sdk/v4/go/aws/ec2"
	"github.com/pulumi/pulumi-aws/sdk/v4/go/aws/iam"
	"github.com/pulumi/pulumi-eks/sdk/go/eks"
	k8s "github.com/pulumi/pulumi-kubernetes/sdk/v3/go/kubernetes"
	corev1 "github.com/pulumi/pulumi-kubernetes/sdk/v3/go/kubernetes/core/v1"
	"github.com/pulumi/pulumi-kubernetes/sdk/v3/go/kubernetes/helm/v2"
	metav1 "github.com/pulumi/pulumi-kubernetes/sdk/v3/go/kubernetes/meta/v1"
	"github.com/pulumi/pulumi-kubernetes/sdk/v3/go/kubernetes/yaml"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {

		// Per NodeGroup IAM: each NodeGroup will bring its own, specific instance role and profile.
		managedPolicyArns := []string{
			"arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy",
			"arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy",
			"arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly",
		}

		// Creates a role and attaches the EKS worker node IAM managed policies. Used a few times below,
		// to create multiple roles, so we use a function to avoid repeating ourselves.
		createRole := func(name string) (*iam.Role, error) {
			instance_assume_role_policy, err := iam.GetPolicyDocument(ctx, &iam.GetPolicyDocumentArgs{
				Statements: []iam.GetPolicyDocumentStatement{
					{
						Actions: []string{
							"sts:AssumeRole",
						},
						Principals: []iam.GetPolicyDocumentStatementPrincipal{
							{
								Type: "Service",
								Identifiers: []string{
									"ec2.amazonaws.com",
								},
							},
						},
					},
				},
			}, nil)
			if err != nil {
				return nil, err
			}

			role, err := iam.NewRole(ctx, name, &iam.RoleArgs{
				AssumeRolePolicy: pulumi.String(instance_assume_role_policy.Json),
				Name:             pulumi.String(name),
			})
			if err != nil {
				return nil, err
			}

			counter := 0
			for _, policy := range managedPolicyArns {
				// Create RolePolicyAttachment without returning it.
				_, err := iam.NewRolePolicyAttachment(ctx,
					fmt.Sprintf("%s-policy-%d", name, counter),
					&iam.RolePolicyAttachmentArgs{
						PolicyArn: pulumi.String(policy),
						Role:      role.Name,
					},
				)
				if err != nil {
					return nil, err
				}
				counter++
			}

			return role, nil
		}

		jsonFile, err := ioutil.ReadFile("elb-policy.json")
		if err != nil {
			return err
		}

		json0 := string(jsonFile)
		_, err = iam.NewPolicy(ctx, "AWSLoadBalancerControllerIAMPolicy", &iam.PolicyArgs{
			Path:        pulumi.String("/"),
			Description: pulumi.String("AWSLoadBalancerControllerIAMPolicy"),
			Policy:      pulumi.String(json0),
		})
		if err != nil {
			return err
		}

		// Read back the default VPC and public subnets, which we will use.
		t := true
		vpc, err := ec2.LookupVpc(ctx, &ec2.LookupVpcArgs{Default: &t})
		if err != nil {
			return err
		}
		subnet, err := ec2.GetSubnetIds(ctx, &ec2.GetSubnetIdsArgs{VpcId: vpc.Id})
		if err != nil {
			return err
		}

		eksClusters := []string{
			"test",
			"prod",
		}

		for _, env := range eksClusters {

			role, err := createRole(fmt.Sprintf("%s-node-role", env))
			if err != nil {
				return err
			}
			_, err = iam.NewInstanceProfile(ctx, fmt.Sprintf("%s-instance-profile", env),
				&iam.InstanceProfileArgs{Role: role})
			if err != nil {
				return err
			}

			// Create an EKS cluster with the many IAM roles to register with the cluster auth.
			cluster, err := eks.NewCluster(ctx, fmt.Sprintf("%s-aws-demo", env), &eks.ClusterArgs{
				SkipDefaultNodeGroup: pulumi.Bool(true),
				CreateOidcProvider:   pulumi.Bool(true),
				VpcId:                pulumi.String(vpc.Id),
				SubnetIds:            toPulumiStringArray(subnet.Ids),
			})
			if err != nil {
				return err
			}

			// Create a Kubernetes provider using the new cluster's Kubeconfig.
			eksProvider, err := k8s.NewProvider(ctx, fmt.Sprintf("%s-eksProvider", env), &k8s.ProviderArgs{
				Kubeconfig: cluster.Kubeconfig.ApplyT(
					func(config interface{}) (string, error) {
						b, err := json.Marshal(config)
						if err != nil {
							return "", err
						}
						return string(b), nil
					}).(pulumi.StringOutput),
			})
			if err != nil {
				return err
			}
			eksProviders := pulumi.ProviderMap(map[string]pulumi.ProviderResource{
				"kubernetes": eksProvider,
			})

			// First, create a node group for fixed compute.
			_, err = eks.NewNodeGroup(ctx, fmt.Sprintf("%s-aws-demo-ng1", env), &eks.NodeGroupArgs{
				Cluster:         cluster.Core,
				InstanceType:    pulumi.String("t2.small"),
				DesiredCapacity: pulumi.Int(3),
				MinSize:         pulumi.Int(1),
				MaxSize:         pulumi.Int(3),
				// Labels: pulumi.StringMap{
				// 	"ondemand": pulumi.String("true"),
				// },
				// InstanceProfile: instanceProfile,
			}, eksProviders)
			if err != nil {
				return err
			}

			argocdNamespace, err := corev1.NewNamespace(ctx, fmt.Sprintf("%s-argocd-ns", env), &corev1.NamespaceArgs{
				Metadata: &metav1.ObjectMetaArgs{
					Name: pulumi.String("argocd"),
				},
			}, pulumi.Provider(eksProvider))
			if err != nil {
				return err
			}

			_, err = helm.NewChart(ctx, fmt.Sprintf("%s-argo-cd", env), helm.ChartArgs{
				Chart:          pulumi.String("argo-cd"),
				Namespace:      pulumi.String("argocd"),
				ResourcePrefix: env,
				FetchArgs: helm.FetchArgs{
					Repo: pulumi.String("https://argoproj.github.io/argo-helm"),
				},
				Values: pulumi.Map{
					"server": pulumi.Map{
						"service": pulumi.Map{
							"type": pulumi.String("LoadBalancer"),
						},
					},
				},
			}, pulumi.Provider(eksProvider), pulumi.DependsOn([]pulumi.Resource{argocdNamespace}))
			if err != nil {
				return err
			}

			_, err = helm.NewChart(ctx, fmt.Sprintf("%s-argo-rollouts", env), helm.ChartArgs{
				Chart:          pulumi.String("argo-rollouts"),
				Namespace:      pulumi.String("argocd"),
				ResourcePrefix: env,
				FetchArgs: helm.FetchArgs{
					Repo: pulumi.String("https://argoproj.github.io/argo-helm"),
				},
				Values: pulumi.Map{
					"dashboard": pulumi.Map{
						"enabled": pulumi.String("true"),
					},
				},
			}, pulumi.Provider(eksProvider), pulumi.DependsOn([]pulumi.Resource{argocdNamespace}))
			if err != nil {
				return err
			}

			_, err = corev1.NewNamespace(ctx, fmt.Sprintf("%s-app-ns", env), &corev1.NamespaceArgs{
				Metadata: &metav1.ObjectMetaArgs{
					Name: pulumi.String(fmt.Sprintf("%s-app", env)),
				},
			}, pulumi.Provider(eksProvider))
			if err != nil {
				return err
			}

			// Export the cluster's kubeconfig.
			ctx.Export(fmt.Sprintf("%s-kubeconfig", env), cluster.Kubeconfig)

			_, err = corev1.NewServiceAccount(ctx, fmt.Sprintf("%s-iam-serviceaccount", env), &corev1.ServiceAccountArgs{
				Metadata: &metav1.ObjectMetaArgs{
					Name:      pulumi.String("aws-load-balancer-controller"),
					Namespace: pulumi.String("kube-system"),
					Annotations: pulumi.StringMap{
						"eks.amazonaws.com/role-arn": pulumi.String("arn:aws:iam::policy/AWSLoadBalancerControllerIAMPolicy"),
					},
				},
			}, pulumi.Provider(eksProvider))
			if err != nil {
				return err
			}

			_, err = yaml.NewConfigFile(ctx, fmt.Sprintf("%s-elb-crd", env), &yaml.ConfigFileArgs{
				File:           "aws-elb-crd.yaml",
				ResourcePrefix: env,
			}, pulumi.Provider(eksProvider))
			if err != nil {
				return err
			}

			_, err = helm.NewChart(ctx, fmt.Sprintf("%s-aws-elb", env), helm.ChartArgs{
				Chart:          pulumi.String("aws-load-balancer-controller"),
				Namespace:      pulumi.String("kube-system"),
				ResourcePrefix: env,
				FetchArgs: helm.FetchArgs{
					Repo: pulumi.String("https://aws.github.io/eks-charts"),
				},
				Values: pulumi.Map{
					"clusterName": cluster.Core,
					"serviceAccount": pulumi.Map{
						"create": pulumi.Bool(false),
						"name":   pulumi.String("aws-load-balancer-controller"),
					},
					"image": pulumi.Map{
						"tag": pulumi.String("v2.3.0"),
					},
				},
			}, pulumi.Provider(eksProvider))
			if err != nil {
				return err
			}

		}
		return nil
	})
}

func toPulumiStringArray(a []string) pulumi.StringArrayInput {
	var res []pulumi.StringInput
	for _, s := range a {
		res = append(res, pulumi.String(s))
	}
	return pulumi.StringArray(res)
}
