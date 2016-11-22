package autospotting

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/davecgh/go-spew/spew"
)

type autoScalingGroup struct {
	*autoscaling.Group

	name   string
	region *region

	instances instances

	// spot instance requests generated for the current group
	spotInstanceRequests []*ec2.SpotInstanceRequest
}

func (a *autoScalingGroup) process() {

	logger.Println("Finding spot instance requests created for", a.name)
	a.findSpotInstanceRequests()
	a.scanInstances()

	debug.Println("Found spot instance requests:", a.spotInstanceRequests)

	spotInstanceID, waitForNextRun := a.havingReadyToAttachSpotInstance()

	if waitForNextRun == true {
		logger.Println("Waiting for next run while processing", a.name)
		return
	}

	if spotInstanceID != nil {
		logger.Println(a.region.name, "Attaching spot instance",
			*spotInstanceID, "to", a.name)

		a.replaceOnDemandInstanceWithSpot(spotInstanceID)
	} else {
		// find any given on-demand instance and try to replace it with a spot one
		onDemandInstance := a.getInstance(nil, true)

		if onDemandInstance == nil {
			logger.Println(a.region.name, a.name,
				"No running on-demand instances were found, nothing to do here...")
			return
		}

		azToLaunchSpotIn := onDemandInstance.Placement.AvailabilityZone
		logger.Println(a.region.name, a.name,
			"Would launch a spot instance in ", *azToLaunchSpotIn)

		a.launchCheapestSpotInstance(azToLaunchSpotIn)
	}
}

func (a *autoScalingGroup) findSpotInstanceRequests() error {

	resp, err := a.region.services.ec2.DescribeSpotInstanceRequests(
		&ec2.DescribeSpotInstanceRequestsInput{
			Filters: []*ec2.Filter{
				{
					Name:   aws.String("tag:launched-for-asg"),
					Values: []*string{a.AutoScalingGroupName},
				},
			},
		})

	if err != nil {
		return err
	}
	logger.Println("Spot instance requests were previously created for", a.name)
	a.spotInstanceRequests = resp.SpotInstanceRequests
	return nil
}

func (a *autoScalingGroup) scanInstances() {

	logger.Println("Adding instances to", a.name)
	a.instances.catalog = make(map[string]*instance)

	for _, inst := range a.Instances {
		i := a.region.instances.get(*inst.InstanceId)
		debug.Println(i)

		if i.isSpot() {
			i.price = i.typeInfo.pricing.spot[*i.Placement.AvailabilityZone]
		} else {
			i.price = i.typeInfo.pricing.onDemand
		}

		a.instances.add(i)
	}

}

func (a *autoScalingGroup) replaceOnDemandInstanceWithSpot(
	spotInstanceID *string) {

	minSize, maxSize := *a.MinSize, *a.MaxSize
	desiredCapacity := *a.DesiredCapacity

	// temporarily increase AutoScaling group in case it's of static size
	if minSize == maxSize {
		logger.Println(a.name, "Temporarily increasing MaxSize")
		a.setAutoScalingMaxSize(maxSize + 1)
		defer a.setAutoScalingMaxSize(maxSize)
	}

	// get the details of our spot instance so we can see its AZ
	logger.Println(a.name, "Retrieving instance details for ", *spotInstanceID)
	if spotInst := a.region.instances.get(*spotInstanceID); spotInst != nil {

		az := spotInst.Placement.AvailabilityZone

		logger.Println(a.name, *spotInstanceID, "is in the availability zone",
			*az, "looking for an on-demand instance there")

		// find an on-demand instance from the same AZ as our spot instance
		if odInst := a.findOndemandInstanceInAZ(az); odInst != nil {

			logger.Println(a.name, "found on-demand instance", *odInst.InstanceId,
				"replacing with new spot instance", *spotInst.InstanceId)

			// revert attach/detach order when running on minimum capacity
			if desiredCapacity == minSize {
				a.attachSpotInstance(spotInstanceID)
			} else {
				defer a.attachSpotInstance(spotInstanceID)
			}

			a.detachAndTerminateOnDemandInstance(odInst.InstanceId)
		} else {
			logger.Println(a.name, "found no on-demand instances that could be",
				"replaced with the new spot instance", *spotInst.InstanceId,
				"terminating the spot instance.")
			si := a.region.instances.get(*spotInst.InstanceId)
			si.terminate(a.region.services.ec2)

		}
	}
}

// Returns the information about the first running instance found in
// the group, while iterating over all instances from the
// group. It can also filter by AZ and Lifecycle.
func (a *autoScalingGroup) getInstance(
	availabilityZone *string,
	onDemandOnly bool) *instance {

	for _, i := range a.instances.catalog {

		// instance is running
		if *i.State.Name == "running" {

			// the InstanceLifecycle attribute is non-nil only for spot instances,
			// where it contains the value "spot", if we're looking for on-demand
			// instances only, then we have to skip the current instance.
			if onDemandOnly && i.isSpot() {
				continue
			}
			if (availabilityZone != nil) &&
				(*availabilityZone != *i.Placement.AvailabilityZone) {
				continue
			}
			return i
		}
	}
	return nil
}

func (a *autoScalingGroup) findOndemandInstanceInAZ(az *string) *instance {
	return a.getInstance(az, true)
}

func (a *autoScalingGroup) getAnyOnDemandInstance() *instance {
	return a.getInstance(nil, true)
}

func (a *autoScalingGroup) getAnyInstance() *instance {
	return a.getInstance(nil, false)
}

// returns an instance ID as *string and a bool that tells us if  we need to
// wait for the next run in case there are spot instances still being launched
func (a *autoScalingGroup) havingReadyToAttachSpotInstance() (*string, bool) {

	var activeSpotInstanceRequest *ec2.SpotInstanceRequest

	// if there are on-demand instances but no spot instance requests yet,
	// then we can launch a new spot instance
	if len(a.spotInstanceRequests) == 0 {
		logger.Println(a.name, "no spot bids were found")
		if inst := a.getAnyOnDemandInstance(); inst != nil {
			logger.Println(a.name, "on-demand instances were found, proceeding to "+
				"launch a replacement spot instance")
			return nil, false
		}
		// Looks like we have no instances in the group, so we can stop here
		logger.Println(a.name, "no on-demand instances were found, nothing to do")
		return nil, true
	}

	logger.Println("spot bids were found, continuing")

	// Here we search for open spot requests created for the current ASG, and try
	// to wait for their instances to start.
	for _, req := range a.spotInstanceRequests {
		if *req.State == "open" && *req.Tags[0].Value == a.name {
			logger.Println(a.name, "Open bid found for current AutoScaling Group, "+
				"waiting for the instance to start so it can be tagged...")

			// Here we resume the wait for instances, initiated after requesting the
			// spot instance. This may sometimes time out the entire lambda function
			// run, just like it could time out the one done when we requested the
			// new instance. In case of timeout the next run should continue waiting
			// for the instance, and the process should continue until the new
			// instance was found. In case of failed spot requests, the first lambda
			// function timeout when waiting for the instances would break the loop,
			// because the subsequent run would find a failed spot request instead
			// of an open one.
			a.waitForAndTagSpotInstance(req)
			activeSpotInstanceRequest = req
		}

		// We found a spot request with a running instance.
		if *req.State == "active" &&
			*req.Status.Code == "fulfilled" {
			logger.Println(a.name, "Active bid was found, with instance already "+
				"started:", *req.InstanceId)

			// If the instance is already in the group we don't need to do anything.
			if a.instances.get(*req.InstanceId) != nil {
				logger.Println(a.name, "Instance", *req.InstanceId,
					"is already attached to the ASG, skipping...")
				continue

				// In case the instance wasn't yet attached, we prepare to attach it.
			} else {
				logger.Println(a.name, "Instance", *req.InstanceId,
					"is not yet attached to the ASG, checking if it's running")

				if i := a.instances.get(*req.InstanceId); i != nil &&
					i.State != nil &&
					*i.State.Name == "running" {
					logger.Println(a.name, "Active bid was found, with running "+
						"instances not yet attached to the ASG",
						*req.InstanceId)
					activeSpotInstanceRequest = req
					break
				} else {
					logger.Println(a.name, "Active bid was found, with no running "+
						"instances, waiting for an instance to start ...")
					a.waitForAndTagSpotInstance(req)
					activeSpotInstanceRequest = req
				}
			}
		}
	}

	// In case we don't have any active spot requests with instances in the
	// process of starting or already ready to be attached to the group, we can
	// launch a new spot instance.
	if activeSpotInstanceRequest == nil {
		logger.Println(a.name, "No active unfulfilled bid was found")
		return nil, false
	}

	spotInstanceID := activeSpotInstanceRequest.InstanceId

	logger.Println("Considering ", *spotInstanceID, "for attaching to", a.name)

	instData := a.region.instances.get(*spotInstanceID)
	gracePeriod := *a.HealthCheckGracePeriod

	debug.Println(instData)

	if instData == nil || instData.LaunchTime == nil {
		logger.Println("Apparently", *spotInstanceID, "is no longer running, moving on...")
		return nil, true
	}

	instanceUpTime := time.Now().Unix() - instData.LaunchTime.Unix()

	logger.Println("Instance uptime:", time.Duration(instanceUpTime)*time.Second)

	// Check if the spot instance is out of the grace period, so in that case we
	// can replace an on-demand instance with it
	if *instData.State.Name == "running" &&
		instanceUpTime < gracePeriod {
		logger.Println("The new spot instance", *spotInstanceID,
			"is still in the grace period,",
			"waiting for it to be ready before we can attach it to the group...")
		return nil, true
	}
	return spotInstanceID, false
}

// This function returns an Instance ID
func (a *autoScalingGroup) waitForAndTagSpotInstance(
	spotRequest *ec2.SpotInstanceRequest) {

	logger.Println(a.name, "Waiting for spot instance for spot instance request",
		*spotRequest.SpotInstanceRequestId)

	ec2Client := a.region.services.ec2

	params := ec2.DescribeSpotInstanceRequestsInput{
		SpotInstanceRequestIds: []*string{spotRequest.SpotInstanceRequestId},
	}

	err := ec2Client.WaitUntilSpotInstanceRequestFulfilled(&params)
	if err != nil {
		logger.Println(a.name, "Error waiting for instance:", err.Error())
		return
	}

	logger.Println(a.name, "Done waiting for an instance.")

	// Now we try to get the InstanceID of the instance we got
	requestDetails, err := ec2Client.DescribeSpotInstanceRequests(&params)
	if err != nil {
		logger.Println(a.name, "Failed to describe spot instance requests")
	}

	// due to the waiter we can now safely assume all this data is available
	spotInstanceID := requestDetails.SpotInstanceRequests[0].InstanceId

	logger.Println(a.name, "found new spot instance", *spotInstanceID,
		"\nTagging it to match the other instances from the group")
	a.region.tagInstance(spotInstanceID, a.getAnyInstance().filterTags())
}

func (a *autoScalingGroup) launchCheapestSpotInstance(azToLaunchIn *string) {

	if azToLaunchIn == nil {
		logger.Println("Can't launch instances in any AZ, nothing to do here...")
		return
	}

	logger.Println("Trying to launch spot instance in", *azToLaunchIn,
		"\nfirst finding an on-demand instance to use as a template")

	baseInstance := a.findOndemandInstanceInAZ(azToLaunchIn)

	if baseInstance == nil {
		logger.Println("Found no on-demand instances, nothing to do here...")
		return
	}
	logger.Println("Found on-demand instance", *baseInstance.InstanceId)

	newInstanceType, err := a.getCheapestCompatibleSpotInstanceType(
		*azToLaunchIn,
		baseInstance)

	if newInstanceType == nil {
		logger.Println("No cheaper compatible instance type was found, "+
			"nothing to do here...", err)
		return
	}

	baseOnDemandPrice := baseInstance.price

	currentSpotPrice := a.region.
		instanceTypeInformation[*newInstanceType].pricing.spot[*azToLaunchIn]

	logger.Println("Finished searching for best spot instance in ",
		*azToLaunchIn,
		"\nreplacing an on-demand", *baseInstance.InstanceType,
		"instance having the ondemand price", baseOnDemandPrice,
		"\nLaunching best compatible instance:", *newInstanceType,
		"with current spot price:", currentSpotPrice)

	lc := a.getLaunchConfiguration()

	spotLS := convertLaunchConfigurationToSpotSpecification(
		lc,
		baseInstance,
		*newInstanceType,
		*azToLaunchIn)

	logger.Println("Bidding for spot instance for ", a.name)
	a.bidForSpotInstance(spotLS, baseOnDemandPrice)
}

func (a *autoScalingGroup) setAutoScalingMaxSize(maxSize int64) {
	svc := a.region.services.autoScaling

	_, err := svc.UpdateAutoScalingGroup(
		&autoscaling.UpdateAutoScalingGroupInput{
			AutoScalingGroupName: aws.String(a.name),
			MaxSize:              aws.Int64(maxSize),
		})

	if err != nil {
		// Print the error, cast err to awserr.Error to get the Code and
		// Message from an error.
		logger.Println(err.Error())
		return
	}
}

func (a *autoScalingGroup) bidForSpotInstance(
	ls *ec2.RequestSpotLaunchSpecification,
	price float64) {

	svc := a.region.services.ec2

	resp, err := svc.RequestSpotInstances(&ec2.RequestSpotInstancesInput{
		SpotPrice:           aws.String(strconv.FormatFloat(price, 'f', -1, 64)),
		LaunchSpecification: ls,
	})

	if err != nil {
		logger.Println("Failed to create spot instance request for",
			a.name, err.Error(), ls)
		return
	}

	spotRequest := resp.SpotInstanceRequests[0]
	spotRequestID := spotRequest.SpotInstanceRequestId

	logger.Println(a.name, "Created spot instance request", *spotRequestID)

	// tag the spot instance request to associate it with the current ASG, so we
	// know where to attach the instance later. In case the waiter failed, it may
	// happen that the instance is actually tagged in the next run, but the spot
	// instance request needs to be tagged anyway.
	a.tagSpotInstanceRequest(*spotRequestID)

	// Waiting for the instance to start so that we can then later tag it with
	// the same tags originally set on the on-demand instances.
	//
	// This waiter only returns after the instance was found and it may be
	// interrupted by the lambda function's timeout, so we also need to check in
	// the next run if we have any open spot requests with no instances and
	// resume the wait there.
	a.waitForAndTagSpotInstance(spotRequest)
}

func (a *autoScalingGroup) tagSpotInstanceRequest(requestID string) {
	svc := a.region.services.ec2

	_, err := svc.CreateTags(&ec2.CreateTagsInput{
		Resources: []*string{aws.String(requestID)},
		Tags: []*ec2.Tag{
			{
				Key:   aws.String("launched-for-asg"),
				Value: aws.String(a.name),
			},
		},
	})

	if err != nil {
		// Print the error, cast err to awserr.Error to get the Code and
		// Message from an error.
		logger.Println(a.name,
			"Failed to create tags for the spot instance request",
			err.Error())
		return
	}

	logger.Println(a.name, "successfully tagged spot instance request", requestID)
}

func (a *autoScalingGroup) getLaunchConfiguration() *autoscaling.LaunchConfiguration {

	lcName := a.LaunchConfigurationName

	if lcName == nil {
		return nil
	}

	svc := a.region.services.autoScaling

	params := &autoscaling.DescribeLaunchConfigurationsInput{
		LaunchConfigurationNames: []*string{lcName},
	}
	resp, err := svc.DescribeLaunchConfigurations(params)

	if err != nil {
		logger.Println(err.Error())
		return nil
	}

	return resp.LaunchConfigurations[0]
}

func convertLaunchConfigurationToSpotSpecification(
	lc *autoscaling.LaunchConfiguration,
	baseInstance *instance,
	instanceType string,
	az string) *ec2.RequestSpotLaunchSpecification {

	var spotLS ec2.RequestSpotLaunchSpecification

	// convert attributes
	spotLS.BlockDeviceMappings = copyBlockDeviceMappings(lc.BlockDeviceMappings)

	if lc.EbsOptimized != nil {
		spotLS.EbsOptimized = lc.EbsOptimized
	}

	// The launch configuration's IamInstanceProfile field can store either a
	// human-friendly ID or an ARN, so we have to see which one is it
	var iamInstanceProfile ec2.IamInstanceProfileSpecification
	if lc.IamInstanceProfile != nil {
		if strings.HasPrefix(*lc.IamInstanceProfile, "arn:aws:") {
			iamInstanceProfile.Arn = lc.IamInstanceProfile
		} else {
			iamInstanceProfile.Name = lc.IamInstanceProfile
		}
		spotLS.IamInstanceProfile = &iamInstanceProfile
	}

	spotLS.ImageId = lc.ImageId

	spotLS.InstanceType = &instanceType

	// these ones should NOT be copied, they break the SpotLaunchSpecification,
	// so that it can't be launched
	// - spotLS.KernelId
	// - spotLS.RamdiskId

	if lc.KeyName != nil && *lc.KeyName != "" {
		spotLS.KeyName = lc.KeyName
	}

	if lc.InstanceMonitoring != nil {
		spotLS.Monitoring = &ec2.RunInstancesMonitoringEnabled{
			Enabled: lc.InstanceMonitoring.Enabled,
		}
	}

	if lc.AssociatePublicIpAddress != nil || baseInstance.SubnetId != nil {
		// Instances are running in a VPC.
		spotLS.NetworkInterfaces = []*ec2.InstanceNetworkInterfaceSpecification{
			&ec2.InstanceNetworkInterfaceSpecification{
				AssociatePublicIpAddress: lc.AssociatePublicIpAddress,
				DeviceIndex:              aws.Int64(0),
				SubnetId:                 baseInstance.SubnetId,
				Groups:                   lc.SecurityGroups,
			},
		}
	} else {
		// Instances are running in EC2 Classic.
		spotLS.SecurityGroups = lc.SecurityGroups
	}

	if lc.UserData != nil && *lc.UserData != "" {
		spotLS.UserData = lc.UserData
	}

	spotLS.Placement = &ec2.SpotPlacement{AvailabilityZone: &az}

	return &spotLS

}

func copyBlockDeviceMappings(
	lcBDMs []*autoscaling.BlockDeviceMapping) []*ec2.BlockDeviceMapping {

	var ec2BDMlist []*ec2.BlockDeviceMapping
	var ec2BDM ec2.BlockDeviceMapping

	for _, lcBDM := range lcBDMs {
		ec2BDM.DeviceName = lcBDM.DeviceName

		// EBS volume information
		if lcBDM.Ebs != nil {
			ec2BDM.Ebs = &ec2.EbsBlockDevice{
				DeleteOnTermination: lcBDM.Ebs.DeleteOnTermination,
				Encrypted:           lcBDM.Ebs.Encrypted,
				Iops:                lcBDM.Ebs.Iops,
				SnapshotId:          lcBDM.Ebs.SnapshotId,
				VolumeSize:          lcBDM.Ebs.VolumeSize,
				VolumeType:          lcBDM.Ebs.VolumeType,
			}
		}

		// it turns out that the noDevice field needs to be converted from bool to
		// *string
		if lcBDM.NoDevice != nil {
			ec2BDM.NoDevice = aws.String(fmt.Sprintf("%t", *lcBDM.NoDevice))
		}

		ec2BDM.VirtualName = lcBDM.VirtualName

		ec2BDMlist = append(ec2BDMlist, &ec2BDM)

	}
	return ec2BDMlist
}

func (a *autoScalingGroup) attachSpotInstance(spotInstanceID *string) {

	svc := a.region.services.autoScaling

	params := autoscaling.AttachInstancesInput{
		AutoScalingGroupName: aws.String(a.name),
		InstanceIds: []*string{
			spotInstanceID,
		},
	}

	resp, err := svc.AttachInstances(&params)

	if err != nil {
		logger.Println(err.Error())
		// Pretty-print the response data.
		logger.Println(resp)
	}

}

// Terminates an on-demand instance from the group,
// but only after it was detached from the autoscaling group
func (a *autoScalingGroup) detachAndTerminateOnDemandInstance(
	instanceID *string) {

	logger.Println(a.region.name,
		a.name,
		"Detaching and terminating instance:",
		*instanceID)

	// detach the on-demand instance
	detachParams := autoscaling.DetachInstancesInput{
		AutoScalingGroupName: aws.String(a.name),
		InstanceIds: []*string{
			instanceID,
		},
		ShouldDecrementDesiredCapacity: aws.Bool(true),
	}

	asSvc := a.region.services.autoScaling

	if _, err := asSvc.DetachInstances(&detachParams); err != nil {
		logger.Println(err.Error())
	}

	a.instances.get(*instanceID).terminate(a.region.services.ec2)

}

func (a *autoScalingGroup) getCheapestCompatibleSpotInstanceType(
	availabilityZone string,
	baseInstance *instance) (*string, error) {

	logger.Println("Getting cheapest spot instance compatible to ",
		*baseInstance.InstanceId, " of type", *baseInstance.InstanceType)

	filteredInstanceTypes, err := a.getCompatibleSpotInstanceTypes(
		availabilityZone,
		baseInstance)

	if err != nil {
		logger.Println("Couldn't find any compatible instance types", err)
		return nil, err
	}

	minPrice := math.MaxFloat64
	var chosenInstanceType string

	for _, instanceType := range filteredInstanceTypes {
		price := a.region.instanceTypeInformation[instanceType].pricing.spot[availabilityZone]

		if price < minPrice {
			minPrice, chosenInstanceType = price, instanceType
			logger.Println(a.name, "changed current minimum to ", minPrice)
		}
		logger.Println(a.name, "cheapest instance type so far is ",
			chosenInstanceType, "priced at", minPrice)
	}

	if chosenInstanceType != "" {
		logger.Println("Chose cheapest instance type", chosenInstanceType)
		return &chosenInstanceType, nil
	}
	logger.Println("Couldn't find any cheaper spot instance type")
	return nil, fmt.Errorf("No cheaper spot instance types could be found")

}

// Why the heck isn't this in the Go standard library?
func min(x, y int) int {
	if x < y {
		return x
	}
	return y
}

func (a *autoScalingGroup) getCompatibleSpotInstanceTypes(
	availabilityZone string, refInstance *instance) ([]string, error) {

	logger.Println("Getting spot instances compatible to ",
		*refInstance.InstanceId, " of type", *refInstance.InstanceType)

	debug.Println("Using this data as reference", spew.Sdump(refInstance))

	var filteredInstanceTypes []string

	existing := refInstance.typeInfo

	debug.Println("Using this data as reference", existing)

	debug.Println("Instance Data", spew.Sdump(a.region.instanceTypeInformation))

	// Count the ephemeral volumes attached to the original instance's block
	// device mappings, this number is used later when comparing with each
	// instance type.
	lcMappings, err := a.countLaunchConfigEphemeralVolumes()

	if err == nil {
		logger.Println("Couldn't determine the launch configuration device mapping",
			"configuration")
	}

	attachedVolumesNumber := min(lcMappings, existing.instanceStoreDeviceCount)

	//filtering compatible instance types
	for _, candidate := range a.region.instanceTypeInformation {

		logger.Println("\nComparing ", candidate, " with ", existing)

		spotPriceNewInstance := candidate.pricing.spot[availabilityZone]

		if spotPriceNewInstance == 0 {
			logger.Println("Missing spot pricing information, skipping",
				candidate.instanceType)
			continue
		}

		if spotPriceNewInstance <= refInstance.price {
			logger.Println("pricing compatible, continuing evaluation: ",
				candidate.pricing.spot[availabilityZone], "<=",
				refInstance.price)
		} else {
			logger.Println("price too high, skipping", candidate.instanceType)
			continue
		}

		if candidate.instanceType == "m4.16xlarge" {
			logger.Println("This is a m4.16xlarge, continuing evaluation")
		} else if candidate.instanceType == "m4.10xlarge" { 
			logger.Println("This is a m4.10xlarge, continuing evaluation")
    	} else if candidate.instanceType == "c4.8xlarge" { 
            logger.Println("This is a c4.8xlarge, continuing evaluation")
        } else if candidate.instanceType == "cc2.8xlarge" { 
            logger.Println("This is a cc2.8xlarge, continuing evaluation")
        } else {
			logger.Println("Not a m4.16xlarge, m4.10xlarge, c4.8xlarge, cc2.8xlarge, skipping", candidate.instanceType)
			continue
		}

		// Here we check the storage compatibility, with the following evaluation
		// criteria:
		// - speed: don't accept spinning disks when we used to have SSDs
		// - number of volumes: the new instance should have enough volumes to be
		//   able to attach all the instance store device mappings defined on the
		//   original instance
		// - volume size: each of the volumes should be at least as big as the
		//   original instance's volumes

		if attachedVolumesNumber > 0 {
			logger.Println("Checking the new instance's ephemeral storage",
				"configuration because the initial instance had attached",
				"ephemeral instance store volumes")

			if candidate.instanceStoreDeviceCount >= attachedVolumesNumber {
				logger.Println("instance store volume count compatible,",
					"continuing	evaluation")
			} else {
				logger.Println("instance store volume count incompatible, skipping",
					candidate.instanceType)
				continue
			}

			if candidate.instanceStoreDeviceSize >= existing.instanceStoreDeviceSize {
				logger.Println("instance store volume size compatible,",
					"continuing evaluation")
			} else {
				logger.Println("instance store volume size incompatible, skipping",
					candidate.instanceType)
				continue
			}

			// Don't accept ephemeral spinning disks if the original instance has
			// ephemeral SSDs, but accept spinning disks if we had those before.
			if candidate.instanceStoreIsSSD ||
				(candidate.instanceStoreIsSSD == existing.instanceStoreIsSSD) {
				logger.Println("instance store type(SSD/spinning) compatible,",
					"continuing evaluation")
			} else {
				logger.Println("instance store type(SSD/spinning) incompatible,",
					"skipping", candidate.instanceType)
				continue
			}
		}

		if compatibleVirtualization(*refInstance.VirtualizationType,
			candidate.virtualizationTypes) {
			logger.Println("virtualization compatible, continuing evaluation")
		} else {
			logger.Println("virtualization incompatible, skipping",
				candidate.instanceType)
			continue
		}

		// checking how many spot instances of this type we already have, so that
		// we can see how risky it is to launch a new one.
		spotInstanceCount := a.alreadyRunningSpotInstanceCount(
			candidate.instanceType, availabilityZone)

		// We skip it in case we have more than 20% instances of this type already
		// running
		if spotInstanceCount == 0 ||
			(*a.DesiredCapacity/spotInstanceCount > 4) {
			logger.Println(a.name,
				"no redundancy issues found for", candidate.instanceType,
				"existing", spotInstanceCount,
				"spot instances, adding for comparison",
			)

			filteredInstanceTypes = append(filteredInstanceTypes, candidate.instanceType)
		} else {
			logger.Println("\nInstances ", candidate, " and ", existing,
				"are not compatible or resulting redundancy for the availability zone",
				"would be dangerously low")

		}

	}
	logger.Printf("\n Found following compatible instances: %#v\n",
		filteredInstanceTypes)
	return filteredInstanceTypes, nil

}

func compatibleVirtualization(virtualizationType string,
	availableVirtualizationTypes []string) bool {

	logger.Println("Available: ", availableVirtualizationTypes,
		"Tested: ", virtualizationType)

	for _, avt := range availableVirtualizationTypes {
		if (avt == "PV") && (virtualizationType == "paravirtual") ||
			(avt == "HVM") && (virtualizationType == "hvm") {
			logger.Println("Compatible")
			return true
		}
	}
	return false
}

func (a *autoScalingGroup) countLaunchConfigEphemeralVolumes() (int, error) {
	count := 0

	lc := a.getLaunchConfiguration()

	if lc == nil {
		return 0, fmt.Errorf("Launch configuration not found")
	}

	if lc.BlockDeviceMappings == nil {
		return 0, fmt.Errorf("Launch configuration has no block device mappings")
	}

	for _, mapping := range lc.BlockDeviceMappings {
		if mapping.VirtualName != nil &&
			strings.Contains(*mapping.VirtualName, "ephemeral") {
			logger.Println("Found ephemeral device mapping", *mapping.VirtualName)
			count++
		}
	}
	logger.Printf("Launch configuration would attach %d ephemeral volumes",
		"if available", count)
	return count, nil
}

// Counts the number of already running spot instances.
func (a *autoScalingGroup) alreadyRunningSpotInstanceCount(
	instanceType, availabilityZone string) int64 {

	var count int64
	logger.Println(a.name, "Counting already running spot instances of type ",
		instanceType, " in AZ ", availabilityZone)
	for _, inst := range a.instances.catalog {
		if *inst.InstanceType == instanceType &&
			*inst.Placement.AvailabilityZone == availabilityZone &&
			inst.isSpot() {
			logger.Println(a.name, "Found running spot instance ",
				*inst.InstanceId, "of the same type:", instanceType)
			count++
		}
	}
	logger.Println(a.name, "Found", count, instanceType, "instances")
	return count
}
