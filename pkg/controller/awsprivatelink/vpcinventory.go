package awsprivatelink

import (
	"strings"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/openshift/hive/pkg/awsclient"
	"k8s.io/apimachinery/pkg/util/sets"

	hivev1 "github.com/openshift/hive/apis/hive/v1"
)

var (
	errNoSupportedAZsInInventory = errors.New("no supported VPC in inventory which support the AZs of the service")
	errNoVPCWithQuotaInInventory = errors.New("no supported VPC in inventory with available quota")
)

func (r *ReconcileAWSPrivateLink) chooseVPCForVPCEndpoint(awsClient awsclient.Client,
	cd *hivev1.ClusterDeployment, vpcEndpointServiceName string,
	logger log.FieldLogger) (*hivev1.AWSPrivateLinkInventory, error) {
	serviceLog := logger.WithField("serviceName", vpcEndpointServiceName)
	// Filter out the VPCs in cluster region.
	candidates := filterVPCInventory(r.controllerconfig.DeepCopy().EndpointVPCInventory, toSupportedRegion(cd.Spec.Platform.AWS.Region))
	if len(candidates) == 0 {
		serviceLog.WithField("region", cd.Spec.Platform.AWS.Region).Error("no supported VPC in inventory")
		return nil, errors.New("no supported VPC in inventory for the cluster")
	}

	// Figure out the AZs supported by the service.
	servicesResp, err := awsClient.DescribeVpcEndpointServices(&ec2.DescribeVpcEndpointServicesInput{
		ServiceNames: aws.StringSlice([]string{vpcEndpointServiceName}),
	})
	if err != nil {
		serviceLog.WithError(err).Error("error getting VPC Endpoint Service in hub account")
		return nil, err
	}

	// Filter candidates that don't have at least one subnet in supported AZs.
	supportedAZSet := sets.NewString(aws.StringValueSlice(servicesResp.ServiceDetails[0].AvailabilityZones)...)
	candidates = filterVPCInventory(candidates, toSupportedSubnets(supportedAZSet))
	if len(candidates) == 0 {
		logger.WithField("region", cd.Spec.Platform.AWS.Region).
			WithField("requiredAZs", supportedAZSet.List()).
			Error(errNoSupportedAZsInInventory.Error())
		return nil, errNoSupportedAZsInInventory
	}

	// Figure out which VPCs have quota available for endpoints.
	vpcs := make([]string, 0, len(candidates))
	endpointsPerVPC := map[string]int{}
	for _, cand := range candidates {
		vpcs = append(vpcs, cand.VPCID)
		endpointsPerVPC[cand.VPCID] = 0
	}
	endpointsResp, err := awsClient.DescribeVpcEndpoints(&ec2.DescribeVpcEndpointsInput{
		Filters: []*ec2.Filter{{Name: aws.String("vpc-id"), Values: aws.StringSlice(vpcs)}},
	})
	if err != nil {
		logger.WithField("vpcs", vpcs).WithError(err).Error("error getting VPC Endpoints in the selected VPCs")
		return nil, err
	}
	for _, vEnd := range endpointsResp.VpcEndpoints {
		vpcID := aws.StringValue(vEnd.VpcId)
		endpointsPerVPC[vpcID] = endpointsPerVPC[vpcID] + 1
	}

	candidates = filterVPCInventory(candidates, toAvailableQuota(endpointsPerVPC))
	if len(candidates) == 0 {
		logger.WithField("vpcs", vpcs).Error(errNoVPCWithQuotaInInventory.Error())
		return nil, errNoVPCWithQuotaInInventory
	}

	return &candidates[0], nil
}

type filterVPCInventoryFn func(hivev1.AWSPrivateLinkInventory) bool

func filterVPCInventory(input []hivev1.AWSPrivateLinkInventory, fn filterVPCInventoryFn) []hivev1.AWSPrivateLinkInventory {
	n := 0
	for _, cand := range input {
		if fn(cand) {
			input[n] = cand
			n++
		}
	}
	input = input[:n]
	return input
}

func toSupportedRegion(region string) filterVPCInventoryFn {
	return func(inv hivev1.AWSPrivateLinkInventory) bool {
		return strings.EqualFold(region, inv.Region)
	}
}

func toSupportedSubnets(azs sets.String) filterVPCInventoryFn {
	return func(inv hivev1.AWSPrivateLinkInventory) bool {
		n := 0
		for _, subnet := range inv.Subnets {
			if azs.Has(subnet.AvailabilityZone) {
				inv.Subnets[n] = subnet
				n++
			}
		}
		inv.Subnets = inv.Subnets[:n]
		if len(inv.Subnets) > 0 {
			return true
		}
		return false
	}
}

const (
	// VPCEndpointPerVPCLimit is a limit on the maximum number of VPC endpoints that can
	// be created in a VPC. This limit is used to filter out any VPCs in the inventory
	// that are already at the this upper limit.
	// The actual limit is 255, but we limit to lower limit.
	VPCEndpointPerVPCLimit = 250
)

func toAvailableQuota(existingPerVPC map[string]int) filterVPCInventoryFn {
	return func(inv hivev1.AWSPrivateLinkInventory) bool {
		avail := VPCEndpointPerVPCLimit - existingPerVPC[inv.VPCID]
		if avail > 0 {
			return true
		}
		return false
	}
}
