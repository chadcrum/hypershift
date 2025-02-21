package aws

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/iam/iamiface"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/util/wait"

	awsutil "github.com/openshift/hypershift/cmd/infra/aws/util"
)

type DestroyIAMOptions struct {
	Region             string
	AWSCredentialsFile string
	AWSKey             string
	AWSSecretKey       string
	InfraID            string
}

func NewDestroyIAMCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "aws",
		Short:        "Destroys AWS instance profile for workers",
		SilenceUsage: true,
	}

	opts := DestroyIAMOptions{
		Region:             "us-east-1",
		AWSCredentialsFile: "",
		InfraID:            "",
	}

	cmd.Flags().StringVar(&opts.AWSCredentialsFile, "aws-creds", opts.AWSCredentialsFile, "Path to an AWS credentials file (required)")
	cmd.Flags().StringVar(&opts.InfraID, "infra-id", opts.InfraID, "Infrastructure ID to use for AWS resources.")
	cmd.Flags().StringVar(&opts.Region, "region", opts.Region, "Region where cluster infra lives")

	cmd.MarkFlagRequired("aws-creds")
	cmd.MarkFlagRequired("infra-id")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if err := opts.DestroyIAM(cmd.Context()); err != nil {
			return err
		}
		log.Info("Successfully destroyed IAM infra")
		return nil
	}

	return cmd
}

func (o *DestroyIAMOptions) Run(ctx context.Context) error {
	return wait.PollImmediateUntil(5*time.Second, func() (bool, error) {
		err := o.DestroyIAM(ctx)
		if err != nil {
			if !awsutil.IsErrorRetryable(err) {
				return false, err
			}
			log.Info("WARNING: error during destroy, will retry", "error", err.Error())
			return false, nil
		}
		return true, nil
	}, ctx.Done())
}

func (o *DestroyIAMOptions) DestroyIAM(ctx context.Context) error {
	awsSession := awsutil.NewSession("cli-destroy-iam")
	awsConfig := awsutil.NewConfig(o.AWSCredentialsFile, o.AWSKey, o.AWSSecretKey, o.Region)
	iamClient := iam.New(awsSession, awsConfig)

	var err error
	err = o.DestroyOIDCResources(ctx, iamClient)
	if err != nil {
		return err
	}
	err = o.DestroyWorkerInstanceProfile(iamClient)
	if err != nil {
		return err
	}
	return nil
}

func (o *DestroyIAMOptions) DestroyOIDCResources(ctx context.Context, iamClient iamiface.IAMAPI) error {
	oidcProviderList, err := iamClient.ListOpenIDConnectProviders(&iam.ListOpenIDConnectProvidersInput{})
	if err != nil {
		return err
	}

	for _, provider := range oidcProviderList.OpenIDConnectProviderList {
		if strings.Contains(*provider.Arn, o.InfraID) {
			_, err := iamClient.DeleteOpenIDConnectProvider(&iam.DeleteOpenIDConnectProviderInput{
				OpenIDConnectProviderArn: provider.Arn,
			})
			if err != nil {
				if aerr, ok := err.(awserr.Error); ok {
					if aerr.Code() != iam.ErrCodeNoSuchEntityException {
						log.Error(aerr, "Error deleting OIDC provider", "providerARN", provider.Arn)
						return aerr
					}
				} else {
					log.Error(err, "Error deleting OIDC provider", "providerARN", provider.Arn)
					return err
				}
			} else {
				log.Info("Deleted OIDC provider", "providerARN", provider.Arn)
			}
			break
		}
	}
	if err = o.DestroyOIDCRole(iamClient, "openshift-ingress"); err != nil {
		return err
	}
	if err = o.DestroyOIDCRole(iamClient, "openshift-image-registry"); err != nil {
		return err
	}
	if err = o.DestroyOIDCRole(iamClient, "aws-ebs-csi-driver-controller"); err != nil {
		return err
	}
	if err = o.DestroyOIDCRole(iamClient, "cloud-controller"); err != nil {
		return err
	}
	if err = o.DestroyOIDCRole(iamClient, "node-pool"); err != nil {
		return err
	}
	if err = o.DestroyOIDCRole(iamClient, "control-plane-operator"); err != nil {
		return err
	}
	if err := o.DestroyOIDCRole(iamClient, "cloud-network-config-controller"); err != nil {
		return err
	}

	return nil
}

// CreateOIDCRole create an IAM Role with a trust policy for the OIDC provider
func (o *DestroyIAMOptions) DestroyOIDCRole(client iamiface.IAMAPI, name string) error {
	roleName := fmt.Sprintf("%s-%s", o.InfraID, name)
	_, err := client.DeleteRolePolicy(&iam.DeleteRolePolicyInput{
		PolicyName: aws.String(roleName),
		RoleName:   aws.String(roleName),
	})
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			if aerr.Code() != iam.ErrCodeNoSuchEntityException {
				log.Error(aerr, "Error deleting role policy", "role", roleName)
				return aerr
			}
		} else {
			log.Error(err, "Error deleting role policy", "role", roleName)
			return err
		}
	} else {
		log.Info("Deleted role policy", "role", roleName)
	}

	_, err = client.DeleteRole(&iam.DeleteRoleInput{
		RoleName: aws.String(roleName),
	})
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			if aerr.Code() != iam.ErrCodeNoSuchEntityException {
				log.Error(aerr, "Error deleting role", "role", roleName)
				return aerr
			}
		} else {
			log.Error(err, "Error deleting role", "role", roleName)
			return err
		}
	} else {
		log.Info("Deleted role", "role", roleName)
	}
	return nil
}

func (o *DestroyIAMOptions) DestroyWorkerInstanceProfile(client iamiface.IAMAPI) error {
	profileName := DefaultProfileName(o.InfraID)
	instanceProfile, err := existingInstanceProfile(client, profileName)
	if err != nil {
		return fmt.Errorf("cannot check for existing instance profile: %w", err)
	}
	if instanceProfile != nil {
		for _, role := range instanceProfile.Roles {
			_, err := client.RemoveRoleFromInstanceProfile(&iam.RemoveRoleFromInstanceProfileInput{
				InstanceProfileName: aws.String(profileName),
				RoleName:            role.RoleName,
			})
			if err != nil {
				return fmt.Errorf("cannot remove role %s from instance profile %s: %w", aws.StringValue(role.RoleName), profileName, err)
			}
			log.Info("Removed role from instance profile", "profile", profileName, "role", aws.StringValue(role.RoleName))
		}
		_, err := client.DeleteInstanceProfile(&iam.DeleteInstanceProfileInput{
			InstanceProfileName: aws.String(profileName),
		})
		if err != nil {
			return fmt.Errorf("cannot delete instance profile %s: %w", profileName, err)
		}
		log.Info("Deleted instance profile", "profile", profileName)
	}
	roleName := fmt.Sprintf("%s-role", profileName)
	policyName := fmt.Sprintf("%s-policy", profileName)
	role, err := existingRole(client, roleName)
	if err != nil {
		return fmt.Errorf("cannot check for existing role: %w", err)
	}
	if role != nil {
		hasPolicy, err := existingRolePolicy(client, roleName, policyName)
		if err != nil {
			return fmt.Errorf("cannot check for existing role policy: %w", err)
		}
		if hasPolicy {
			_, err := client.DeleteRolePolicy(&iam.DeleteRolePolicyInput{
				PolicyName: aws.String(policyName),
				RoleName:   aws.String(roleName),
			})
			if err != nil {
				return fmt.Errorf("cannot delete role policy %s from role %s: %w", policyName, roleName, err)
			}
			log.Info("Deleted role policy", "role", roleName, "policy", policyName)
		}
		_, err = client.DeleteRole(&iam.DeleteRoleInput{
			RoleName: aws.String(roleName),
		})
		if err != nil {
			return fmt.Errorf("cannot delete role %s: %w", roleName, err)
		}
		log.Info("Deleted role", "role", roleName)
	}
	return nil
}
