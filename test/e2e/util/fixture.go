package util

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/arn"
	"github.com/aws/aws-sdk-go/service/resourcegroupstaggingapi"
	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/cmd/cluster/aws"
	"github.com/openshift/hypershift/cmd/cluster/azure"
	"github.com/openshift/hypershift/cmd/cluster/core"
	"github.com/openshift/hypershift/cmd/cluster/none"
	"github.com/openshift/hypershift/cmd/cluster/powervs"
	awsutil "github.com/openshift/hypershift/cmd/infra/aws/util"
	"github.com/openshift/hypershift/cmd/util"
	"github.com/openshift/hypershift/test/e2e/util/dump"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/wait"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// createClusterOpts mutates the cluster creation options according to the
// cluster's platform as necessary to deal with options the test caller doesn't
// know or care about in advance.
//
// TODO: Mutates the input, instead should use a copy of the input options
func createClusterOpts(ctx context.Context, client crclient.Client, hc *hyperv1.HostedCluster, opts *PlatformAgnosticOptions) (*PlatformAgnosticOptions, error) {
	opts.Namespace = hc.Namespace
	opts.Name = hc.Name

	switch hc.Spec.Platform.Type {
	case hyperv1.AWSPlatform:
		opts.InfraID = hc.Name
	case hyperv1.AzurePlatform:
		opts.InfraID = hc.Name
	case hyperv1.PowerVSPlatform:
		opts.InfraID = fmt.Sprintf("%s-infra", hc.Name)
	}

	return opts, nil
}

// createCluster calls the correct cluster create CLI function based on the
// cluster platform.
func createCluster(ctx context.Context, hc *hyperv1.HostedCluster, opts *PlatformAgnosticOptions, outputDir string) error {
	validCoreOpts, err := opts.RawCreateOptions.Validate(ctx)
	if err != nil {
		return fmt.Errorf("failed to validate core options: %w", err)
	}
	coreOpts, err := validCoreOpts.Complete()
	if err != nil {
		return fmt.Errorf("failed to complete core options: %w", err)
	}
	infraFile := filepath.Join(outputDir, "infrastructure.json")
	iamFile := filepath.Join(outputDir, "iam.json")
	manifestsFile := filepath.Join(outputDir, "manifests.yaml")
	switch hc.Spec.Platform.Type {
	case hyperv1.AWSPlatform:
		completer, err := opts.AWSPlatform.Validate(ctx, coreOpts)
		if err != nil {
			return fmt.Errorf("failed to validate AWS platform options: %w", err)
		}
		validOpts := completer.(*aws.ValidatedCreateOptions)

		infraOpts := aws.CreateInfraOptions(validOpts, coreOpts)
		infraOpts.OutputFile = infraFile
		infra, err := infraOpts.CreateInfra(ctx, opts.Log)
		if err != nil {
			return fmt.Errorf("failed to create infra: %w", err)
		}
		if err := infraOpts.Output(infra); err != nil {
			return fmt.Errorf("failed to write infra: %w", err)
		}

		client, err := util.GetClient()
		if err != nil {
			return err
		}
		iamOpts := aws.CreateIAMOptions(validOpts, infra)
		iamOpts.OutputFile = iamFile
		iam, err := iamOpts.CreateIAM(ctx, client)
		if err != nil {
			return fmt.Errorf("failed to create IAM: %w", err)
		}
		if err := iamOpts.Output(iam); err != nil {
			return fmt.Errorf("failed to write IAM: %w", err)
		}

		opts.InfrastructureJSON = infraFile
		opts.AWSPlatform.IAMJSON = iamFile
		return renderCreate(ctx, &opts.RawCreateOptions, &opts.AWSPlatform, manifestsFile)
	case hyperv1.NonePlatform:
		return renderCreate(ctx, &opts.RawCreateOptions, &opts.NonePlatform, manifestsFile)
	case hyperv1.KubevirtPlatform:
		return renderCreate(ctx, &opts.RawCreateOptions, &opts.KubevirtPlatform, manifestsFile)
	case hyperv1.AzurePlatform:
		completer, err := opts.AzurePlatform.Validate(ctx, coreOpts)
		if err != nil {
			return fmt.Errorf("failed to validate Azure platform options: %w", err)
		}
		validOpts := completer.(*azure.ValidatedCreateOptions)

		infraOpts, err := azure.CreateInfraOptions(ctx, validOpts, coreOpts)
		if err != nil {
			return fmt.Errorf("failed to create infra options: %w", err)
		}
		infraOpts.OutputFile = infraFile
		if _, err := infraOpts.Run(ctx, opts.Log); err != nil {
			return fmt.Errorf("failed to create infra: %w", err)
		}

		opts.InfrastructureJSON = infraFile
		return renderCreate(ctx, &opts.RawCreateOptions, &opts.AzurePlatform, manifestsFile)
	case hyperv1.PowerVSPlatform:
		completer, err := opts.PowerVSPlatform.Validate(ctx, coreOpts)
		if err != nil {
			return fmt.Errorf("failed to validate PowerVS platform options: %w", err)
		}
		validOpts := completer.(*powervs.ValidatedCreateOptions)

		infraOpts, infra := powervs.CreateInfraOptions(validOpts, coreOpts)
		infraOpts.OutputFile = infraFile
		if err := infra.SetupInfra(ctx, infraOpts); err != nil {
			return fmt.Errorf("failed to setup infra: %w", err)
		}
		infraOpts.Output(infra)

		opts.InfrastructureJSON = infraFile
		return renderCreate(ctx, &opts.RawCreateOptions, &opts.PowerVSPlatform, manifestsFile)
	default:
		return fmt.Errorf("unsupported platform %s", hc.Spec.Platform.Type)
	}
}

func renderCreate(ctx context.Context, opts *core.RawCreateOptions, platformOpts core.PlatformValidator, outputFile string) error {
	opts.Render = true
	opts.RenderInto = outputFile
	if err := core.CreateCluster(ctx, opts, platformOpts); err != nil {
		return fmt.Errorf("failed to render cluster manifests: %w", err)
	}

	opts.Render = false
	opts.RenderInto = ""
	return core.CreateCluster(ctx, opts, platformOpts)
}

// destroyCluster calls the correct cluster destroy CLI function based on the
// cluster platform and the options used to create the cluster.
func destroyCluster(ctx context.Context, t *testing.T, hc *hyperv1.HostedCluster, createOpts *PlatformAgnosticOptions) error {
	opts := &core.DestroyOptions{
		Namespace:          hc.Namespace,
		Name:               hc.Name,
		InfraID:            createOpts.InfraID,
		ClusterGracePeriod: 15 * time.Minute,
		Log:                NewLogr(t),
	}
	switch hc.Spec.Platform.Type {
	case hyperv1.AWSPlatform:
		opts.AWSPlatform = core.AWSPlatformDestroyOptions{
			BaseDomain:       createOpts.BaseDomain,
			Credentials:      createOpts.AWSPlatform.Credentials,
			PreserveIAM:      false,
			Region:           createOpts.AWSPlatform.Region,
			PostDeleteAction: validateAWSGuestResourcesDeletedFunc(ctx, t, hc.Spec.InfraID, createOpts.AWSPlatform.Credentials.AWSCredentialsFile, createOpts.AWSPlatform.Region),
		}
		return aws.DestroyCluster(ctx, opts)
	case hyperv1.NonePlatform, hyperv1.KubevirtPlatform:
		return none.DestroyCluster(ctx, opts)
	case hyperv1.AzurePlatform:
		opts.AzurePlatform = core.AzurePlatformDestroyOptions{
			CredentialsFile: createOpts.AzurePlatform.CredentialsFile,
			Location:        createOpts.AzurePlatform.Location,
		}
		return azure.DestroyCluster(ctx, opts)
	case hyperv1.PowerVSPlatform:
		opts.PowerVSPlatform = core.PowerVSPlatformDestroyOptions{
			BaseDomain:             createOpts.BaseDomain,
			ResourceGroup:          createOpts.PowerVSPlatform.ResourceGroup,
			Region:                 createOpts.PowerVSPlatform.Region,
			Zone:                   createOpts.PowerVSPlatform.Zone,
			VPCRegion:              createOpts.PowerVSPlatform.VPCRegion,
			CloudInstanceID:        createOpts.PowerVSPlatform.CloudInstanceID,
			CloudConnection:        createOpts.PowerVSPlatform.CloudConnection,
			VPC:                    createOpts.PowerVSPlatform.VPC,
			PER:                    createOpts.PowerVSPlatform.PER,
			TransitGatewayLocation: createOpts.PowerVSPlatform.TransitGatewayLocation,
			TransitGateway:         createOpts.PowerVSPlatform.TransitGateway,
		}
		return powervs.DestroyCluster(ctx, opts)

	default:
		return fmt.Errorf("unsupported cluster platform %s", hc.Spec.Platform.Type)
	}
}

// validateAWSGuestResourcesDeletedFunc waits for 15min or until the guest cluster resources are gone.
func validateAWSGuestResourcesDeletedFunc(ctx context.Context, t *testing.T, infraID, awsCreds, awsRegion string) func() {
	return func() {
		awsSession := awsutil.NewSession("cleanup-validation", awsCreds, "", "", awsRegion)
		awsConfig := awsutil.NewConfig()
		taggingClient := resourcegroupstaggingapi.New(awsSession, awsConfig)

		// Find load balancers, persistent volumes, or s3 buckets belonging to the guest cluster
		err := wait.PollImmediate(5*time.Second, 15*time.Minute, func() (bool, error) {
			// Filter get cluster resources.
			output, err := taggingClient.GetResourcesWithContext(ctx, &resourcegroupstaggingapi.GetResourcesInput{
				ResourceTypeFilters: []*string{
					awssdk.String("elasticloadbalancing:loadbalancer"),
					awssdk.String("ec2:volume"),
					awssdk.String("s3"),
				},
				TagFilters: []*resourcegroupstaggingapi.TagFilter{
					{
						Key:    awssdk.String(clusterTag(infraID)),
						Values: []*string{awssdk.String("owned")},
					},
				},
			})
			if err != nil {
				t.Logf("WARNING: failed to list resources by tag: %v. Not verifying cluster is cleaned up.", err)
				return true, nil
			}

			// Log resources that still exists
			if hasGuestResources(t, output.ResourceTagMappingList) {
				t.Logf("WARNING: found %d remaining resources for guest cluster", len(output.ResourceTagMappingList))
				for i := 0; i < len(output.ResourceTagMappingList); i++ {
					resourceARN, err := arn.Parse(awssdk.StringValue(output.ResourceTagMappingList[i].ResourceARN))
					if err != nil {
						t.Logf("WARNING: failed to parse resource: %v. Not verifying cluster is cleaned up.", err)
						return false, nil
					}
					t.Logf("Resource: %s, tags: %s, service: %s",
						awssdk.StringValue(output.ResourceTagMappingList[i].ResourceARN), resourceTags(output.ResourceTagMappingList[i].Tags), resourceARN.Service)
				}
				return false, nil
			}

			t.Log("SUCCESS: found no remaining guest resources")
			return true, nil
		})

		if err != nil {
			t.Errorf("Failed to wait for infra resources in guest cluster to be deleted: %v", err)
		}
	}
}

func resourceTags(tags []*resourcegroupstaggingapi.Tag) string {
	tagStrings := make([]string, len(tags))
	for i, tag := range tags {
		tagStrings[i] = fmt.Sprintf("%s=%s", awssdk.StringValue(tag.Key), awssdk.StringValue(tag.Value))
	}
	return strings.Join(tagStrings, ",")
}

func hasGuestResources(t *testing.T, resourceTagMappings []*resourcegroupstaggingapi.ResourceTagMapping) bool {
	for _, mapping := range resourceTagMappings {
		resourceARN, err := arn.Parse(awssdk.StringValue(mapping.ResourceARN))
		if err != nil {
			t.Logf("WARNING: failed to parse ARN %s", awssdk.StringValue(mapping.ResourceARN))
			continue
		}
		if resourceARN.Service == "ec2" { // Resource is a volume, check whether it's a PV volume by looking at tags
			for _, tag := range mapping.Tags {
				if awssdk.StringValue(tag.Key) == "kubernetes.io/created-for/pv/name" {
					return true
				}
			}
			continue
		} else {
			return true
		}
	}
	return false
}

func clusterTag(infraID string) string {
	return fmt.Sprintf("kubernetes.io/cluster/%s", infraID)
}

// newClusterDumper returns a function that dumps important diagnostic data for
// a cluster based on the cluster's platform. The output directory will be named
// according to the test name. So, the returned dump function should be called
// at most once per unique test name.
func newClusterDumper(hc *hyperv1.HostedCluster, opts *PlatformAgnosticOptions, artifactDir string) func(ctx context.Context, t *testing.T, dumpGuestCluster bool) error {
	return func(ctx context.Context, t *testing.T, dumpGuestCluster bool) error {
		if len(artifactDir) == 0 {
			t.Logf("Skipping cluster dump because no artifact directory was provided")
			return nil
		}

		switch hc.Spec.Platform.Type {
		case hyperv1.AWSPlatform:
			var dumpErrors []error
			err := dump.DumpMachineConsoleLogs(ctx, hc, opts.AWSPlatform.Credentials, artifactDir)
			if err != nil {
				t.Logf("Failed saving machine console logs; this is nonfatal: %v", err)
			}
			err = dump.DumpHostedCluster(ctx, t, hc, dumpGuestCluster, artifactDir)
			if err != nil {
				dumpErrors = append(dumpErrors, fmt.Errorf("failed to dump hosted cluster: %w", err))
			}
			err = dump.DumpJournals(t, ctx, hc, artifactDir, opts.AWSPlatform.Credentials.AWSCredentialsFile)
			if err != nil {
				t.Logf("Failed to dump machine journals; this is nonfatal: %v", err)
			}
			return utilerrors.NewAggregate(dumpErrors)
		default:
			err := dump.DumpHostedCluster(ctx, t, hc, dumpGuestCluster, artifactDir)
			if err != nil {
				return fmt.Errorf("failed to dump hosted cluster: %w", err)
			}
			return nil
		}
	}
}

func artifactSubdirFor(t *testing.T) string {
	return strings.ReplaceAll(t.Name(), "/", "_")
}
