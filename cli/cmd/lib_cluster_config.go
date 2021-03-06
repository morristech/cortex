/*
Copyright 2020 Cortex Labs, Inc.

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

package cmd

import (
	"fmt"
	"path"
	"path/filepath"
	"regexp"

	"github.com/cortexlabs/cortex/pkg/consts"
	"github.com/cortexlabs/cortex/pkg/lib/aws"
	cr "github.com/cortexlabs/cortex/pkg/lib/configreader"
	"github.com/cortexlabs/cortex/pkg/lib/errors"
	"github.com/cortexlabs/cortex/pkg/lib/files"
	"github.com/cortexlabs/cortex/pkg/lib/pointer"
	"github.com/cortexlabs/cortex/pkg/lib/prompt"
	"github.com/cortexlabs/cortex/pkg/lib/sets/strset"
	s "github.com/cortexlabs/cortex/pkg/lib/strings"
	"github.com/cortexlabs/cortex/pkg/lib/table"
	"github.com/cortexlabs/cortex/pkg/types/clusterconfig"
)

var _cachedClusterConfigRegex = regexp.MustCompile(`^cluster_\S+\.yaml$`)

func cachedClusterConfigPath(clusterName string, region string) string {
	return filepath.Join(_localDir, fmt.Sprintf("cluster_%s_%s.yaml", clusterName, region))
}

func mountedClusterConfigPath(clusterName string, region string) string {
	return filepath.Join("/.cortex", fmt.Sprintf("cluster_%s_%s.yaml", clusterName, region))
}

func existingCachedClusterConfigPaths() []string {
	paths, err := files.ListDir(_localDir, false)
	if err != nil {
		return nil
	}

	var matches []string
	for _, p := range paths {
		if _cachedClusterConfigRegex.MatchString(path.Base(p)) {
			matches = append(matches, p)
		}
	}

	return matches
}

func readCachedClusterConfigFile(clusterConfig *clusterconfig.Config, filePath string) error {
	errs := cr.ParseYAMLFile(clusterConfig, clusterconfig.Validation, filePath)
	if errors.HasError(errs) {
		return errors.FirstError(errs...)
	}

	return nil
}

func readUserClusterConfigFile(clusterConfig *clusterconfig.Config) error {
	errs := cr.ParseYAMLFile(clusterConfig, clusterconfig.UserValidation, _flagClusterConfig)
	if errors.HasError(errs) {
		return errors.FirstError(errs...)
	}

	return nil
}

func getClusterAccessConfig(disallowPrompt bool) (*clusterconfig.AccessConfig, error) {
	accessConfig, err := clusterconfig.DefaultAccessConfig()
	if err != nil {
		return nil, err
	}

	if _flagClusterConfig != "" {
		errs := cr.ParseYAMLFile(accessConfig, clusterconfig.AccessValidation, _flagClusterConfig)
		if errors.HasError(errs) {
			return nil, errors.FirstError(errs...)
		}
	}

	if accessConfig.ClusterName != nil && accessConfig.Region != nil {
		return accessConfig, nil
	}

	cachedPaths := existingCachedClusterConfigPaths()
	if len(cachedPaths) == 1 {
		cachedAccessConfig := &clusterconfig.AccessConfig{}
		cr.ParseYAMLFile(cachedAccessConfig, clusterconfig.AccessValidation, cachedPaths[0])
		if accessConfig.ClusterName == nil {
			accessConfig.ClusterName = cachedAccessConfig.ClusterName
		}
		if accessConfig.Region == nil {
			accessConfig.Region = cachedAccessConfig.Region
		}
	}

	if disallowPrompt {
		if len(cachedPaths) > 1 && (accessConfig.ClusterName == nil || accessConfig.Region == nil) {
			return nil, ErrorClusterAccessConfigOrPromptsRequired()
		}

		if accessConfig.ClusterName == nil {
			accessConfig.ClusterName = pointer.String("cortex")
		}
		if accessConfig.Region == nil {
			accessConfig.Region = pointer.String("us-west-2")
		}
		return accessConfig, nil
	}

	err = cr.ReadPrompt(accessConfig, clusterconfig.AccessPromptValidation)
	if err != nil {
		return nil, err
	}

	return accessConfig, nil
}

func getInstallClusterConfig(awsCreds AWSCredentials, envName string, disallowPrompt bool) (*clusterconfig.Config, error) {
	clusterConfig := &clusterconfig.Config{}

	err := clusterconfig.SetDefaults(clusterConfig)
	if err != nil {
		return nil, err
	}

	if _flagClusterConfig != "" {
		err := readUserClusterConfigFile(clusterConfig)
		if err != nil {
			return nil, err
		}
	}

	err = clusterconfig.RegionPrompt(clusterConfig, disallowPrompt)
	if err != nil {
		return nil, err
	}

	awsClient, err := newAWSClient(*clusterConfig.Region, awsCreds)
	if err != nil {
		return nil, err
	}
	promptIfNotAdmin(awsClient, disallowPrompt)

	err = clusterconfig.InstallPrompt(clusterConfig, awsClient, disallowPrompt)
	if err != nil {
		return nil, err
	}

	clusterConfig.Telemetry, err = readTelemetryConfig()
	if err != nil {
		return nil, err
	}

	err = clusterConfig.Validate(awsClient)
	if err != nil {
		if _flagClusterConfig != "" {
			err = errors.Wrap(err, _flagClusterConfig)
		}
		return nil, err
	}

	confirmInstallClusterConfig(clusterConfig, awsCreds, awsClient, envName, disallowPrompt)

	return clusterConfig, nil
}

func getClusterUpdateConfig(cachedClusterConfig clusterconfig.Config, awsCreds AWSCredentials, disallowPrompt bool) (*clusterconfig.Config, error) {
	userClusterConfig := &clusterconfig.Config{}
	var awsClient *aws.Client

	if _flagClusterConfig == "" {
		if disallowPrompt {
			return nil, ErrorClusterConfigOrPromptsRequired()
		}

		userClusterConfig = &cachedClusterConfig
		err := clusterconfig.UpdatePrompt(userClusterConfig, &cachedClusterConfig, false, disallowPrompt)
		if err != nil {
			return nil, err
		}

		awsClient, err = newAWSClient(*userClusterConfig.Region, awsCreds)
		if err != nil {
			return nil, err
		}
		promptIfNotAdmin(awsClient, disallowPrompt)

	} else {
		err := readUserClusterConfigFile(userClusterConfig)
		if err != nil {
			return nil, err
		}

		userClusterConfig.ClusterName = cachedClusterConfig.ClusterName
		userClusterConfig.Region = cachedClusterConfig.Region
		awsClient, err = newAWSClient(*userClusterConfig.Region, awsCreds)
		if err != nil {
			return nil, err
		}
		promptIfNotAdmin(awsClient, disallowPrompt)

		if userClusterConfig.Bucket != "" && userClusterConfig.Bucket != cachedClusterConfig.Bucket {
			return nil, clusterconfig.ErrorConfigCannotBeChangedOnUpdate(clusterconfig.BucketKey, cachedClusterConfig.Bucket)
		}
		userClusterConfig.Bucket = cachedClusterConfig.Bucket

		if userClusterConfig.LogGroup != "" && userClusterConfig.LogGroup != cachedClusterConfig.LogGroup {
			return nil, clusterconfig.ErrorConfigCannotBeChangedOnUpdate(clusterconfig.LogGroupKey, cachedClusterConfig.LogGroup)
		}
		userClusterConfig.LogGroup = cachedClusterConfig.LogGroup

		if userClusterConfig.InstanceType != nil && *userClusterConfig.InstanceType != *cachedClusterConfig.InstanceType {
			return nil, clusterconfig.ErrorConfigCannotBeChangedOnUpdate(clusterconfig.InstanceTypeKey, *cachedClusterConfig.InstanceType)
		}
		userClusterConfig.InstanceType = cachedClusterConfig.InstanceType

		if len(userClusterConfig.AvailabilityZones) > 0 && !strset.New(userClusterConfig.AvailabilityZones...).IsEqual(strset.New(cachedClusterConfig.AvailabilityZones...)) {
			return nil, clusterconfig.ErrorConfigCannotBeChangedOnUpdate(clusterconfig.AvailabilityZonesKey, cachedClusterConfig.AvailabilityZones)
		}
		userClusterConfig.AvailabilityZones = cachedClusterConfig.AvailabilityZones

		if userClusterConfig.InstanceVolumeSize != cachedClusterConfig.InstanceVolumeSize {
			return nil, clusterconfig.ErrorConfigCannotBeChangedOnUpdate(clusterconfig.InstanceVolumeSizeKey, cachedClusterConfig.InstanceVolumeSize)
		}
		userClusterConfig.InstanceVolumeSize = cachedClusterConfig.InstanceVolumeSize

		if userClusterConfig.InstanceVolumeType != cachedClusterConfig.InstanceVolumeType {
			return nil, clusterconfig.ErrorConfigCannotBeChangedOnUpdate(clusterconfig.InstanceVolumeTypeKey, cachedClusterConfig.InstanceVolumeType)
		}
		userClusterConfig.InstanceVolumeType = cachedClusterConfig.InstanceVolumeType

		if userClusterConfig.InstanceVolumeIOPS != cachedClusterConfig.InstanceVolumeIOPS {
			return nil, clusterconfig.ErrorConfigCannotBeChangedOnUpdate(clusterconfig.InstanceVolumeIOPSKey, cachedClusterConfig.InstanceVolumeIOPS)
		}
		userClusterConfig.InstanceVolumeIOPS = cachedClusterConfig.InstanceVolumeIOPS

		if userClusterConfig.Spot != nil && *userClusterConfig.Spot != *cachedClusterConfig.Spot {
			return nil, clusterconfig.ErrorConfigCannotBeChangedOnUpdate(clusterconfig.SpotKey, *cachedClusterConfig.Spot)
		}
		userClusterConfig.Spot = cachedClusterConfig.Spot

		if userClusterConfig.Spot != nil && *userClusterConfig.Spot {
			err = userClusterConfig.FillEmptySpotFields(awsClient)
			if err != nil {
				return nil, err
			}
		}

		if userClusterConfig.SpotConfig != nil && s.Obj(userClusterConfig.SpotConfig) != s.Obj(cachedClusterConfig.SpotConfig) {
			if cachedClusterConfig.SpotConfig == nil {
				return nil, clusterconfig.ErrorConfiguredWhenSpotIsNotEnabled(clusterconfig.SpotConfigKey)
			}

			if !strset.New(userClusterConfig.SpotConfig.InstanceDistribution...).IsEqual(strset.New(cachedClusterConfig.SpotConfig.InstanceDistribution...)) {
				return nil, errors.Wrap(clusterconfig.ErrorConfigCannotBeChangedOnUpdate(clusterconfig.InstanceDistributionKey, cachedClusterConfig.SpotConfig.InstanceDistribution), clusterconfig.SpotConfigKey)
			}

			if userClusterConfig.SpotConfig.OnDemandBaseCapacity != nil && *userClusterConfig.SpotConfig.OnDemandBaseCapacity != *cachedClusterConfig.SpotConfig.OnDemandBaseCapacity {
				return nil, errors.Wrap(clusterconfig.ErrorConfigCannotBeChangedOnUpdate(clusterconfig.OnDemandBaseCapacityKey, *cachedClusterConfig.SpotConfig.OnDemandBaseCapacity), clusterconfig.SpotConfigKey)
			}

			if userClusterConfig.SpotConfig.OnDemandPercentageAboveBaseCapacity != nil && *userClusterConfig.SpotConfig.OnDemandPercentageAboveBaseCapacity != *cachedClusterConfig.SpotConfig.OnDemandPercentageAboveBaseCapacity {
				return nil, errors.Wrap(clusterconfig.ErrorConfigCannotBeChangedOnUpdate(clusterconfig.OnDemandPercentageAboveBaseCapacityKey, *cachedClusterConfig.SpotConfig.OnDemandPercentageAboveBaseCapacity), clusterconfig.SpotConfigKey)
			}

			if userClusterConfig.SpotConfig.MaxPrice != nil && *userClusterConfig.SpotConfig.MaxPrice != *cachedClusterConfig.SpotConfig.MaxPrice {
				return nil, errors.Wrap(clusterconfig.ErrorConfigCannotBeChangedOnUpdate(clusterconfig.MaxPriceKey, *cachedClusterConfig.SpotConfig.MaxPrice), clusterconfig.SpotConfigKey)
			}

			if userClusterConfig.SpotConfig.InstancePools != nil && *userClusterConfig.SpotConfig.InstancePools != *cachedClusterConfig.SpotConfig.InstancePools {
				return nil, errors.Wrap(clusterconfig.ErrorConfigCannotBeChangedOnUpdate(clusterconfig.InstancePoolsKey, *cachedClusterConfig.SpotConfig.InstancePools), clusterconfig.SpotConfigKey)
			}

			if userClusterConfig.SpotConfig.OnDemandBackup != cachedClusterConfig.SpotConfig.OnDemandBackup {
				return nil, errors.Wrap(clusterconfig.ErrorConfigCannotBeChangedOnUpdate(clusterconfig.OnDemandBackupKey, cachedClusterConfig.SpotConfig.OnDemandBackup), clusterconfig.SpotConfigKey)
			}
		}
		userClusterConfig.SpotConfig = cachedClusterConfig.SpotConfig

		err = clusterconfig.UpdatePrompt(userClusterConfig, &cachedClusterConfig, true, disallowPrompt)
		if err != nil {
			return nil, err
		}
	}

	var err error
	userClusterConfig.Telemetry, err = readTelemetryConfig()
	if err != nil {
		return nil, err
	}

	err = userClusterConfig.Validate(awsClient)
	if err != nil {
		if _flagClusterConfig != "" {
			err = errors.Wrap(err, _flagClusterConfig)
		}
		return nil, err
	}

	confirmUpdateClusterConfig(*userClusterConfig, awsCreds, awsClient, disallowPrompt)

	return userClusterConfig, nil
}

func confirmInstallClusterConfig(clusterConfig *clusterconfig.Config, awsCreds AWSCredentials, awsClient *aws.Client, envName string, disallowPrompt bool) {
	eksPrice := aws.EKSPrices[*clusterConfig.Region]
	operatorInstancePrice := aws.InstanceMetadatas[*clusterConfig.Region]["t3.medium"].Price
	operatorEBSPrice := aws.EBSMetadatas[*clusterConfig.Region]["gp2"].PriceGB * 20 / 30 / 24
	elbPrice := aws.ELBMetadatas[*clusterConfig.Region].Price
	apiInstancePrice := aws.InstanceMetadatas[*clusterConfig.Region][*clusterConfig.InstanceType].Price
	apiEBSPrice := aws.EBSMetadatas[*clusterConfig.Region][clusterConfig.InstanceVolumeType.String()].PriceGB * float64(clusterConfig.InstanceVolumeSize) / 30 / 24
	if clusterConfig.InstanceVolumeType.String() == "io1" && clusterConfig.InstanceVolumeIOPS != nil {
		apiEBSPrice += aws.EBSMetadatas[*clusterConfig.Region][clusterConfig.InstanceVolumeType.String()].PriceIOPS * float64(*clusterConfig.InstanceVolumeIOPS) / 30 / 24
	}
	fixedPrice := eksPrice + operatorInstancePrice + operatorEBSPrice + 2*elbPrice
	totalMinPrice := fixedPrice + float64(*clusterConfig.MinInstances)*(apiInstancePrice+apiEBSPrice)
	totalMaxPrice := fixedPrice + float64(*clusterConfig.MaxInstances)*(apiInstancePrice+apiEBSPrice)

	fmt.Printf("aws access key id %s will be used to provision a cluster (%s) in %s:\n\n", s.MaskString(awsCreds.AWSAccessKeyID, 4), clusterConfig.ClusterName, *clusterConfig.Region)

	headers := []table.Header{
		{Title: "aws resource"},
		{Title: "cost per hour"},
	}

	rows := [][]interface{}{}
	rows = append(rows, []interface{}{"1 eks cluster", s.DollarsMaxPrecision(eksPrice)})

	instanceStr := "instances"
	volumeStr := "volumes"
	if *clusterConfig.MinInstances == 1 && *clusterConfig.MaxInstances == 1 {
		instanceStr = "instance"
		volumeStr = "volume"
	}
	workerInstanceStr := fmt.Sprintf("%d - %d %s %s for your apis", *clusterConfig.MinInstances, *clusterConfig.MaxInstances, *clusterConfig.InstanceType, instanceStr)
	ebsInstanceStr := fmt.Sprintf("%d - %d %dgb ebs %s for your apis", *clusterConfig.MinInstances, *clusterConfig.MaxInstances, clusterConfig.InstanceVolumeSize, volumeStr)
	if *clusterConfig.MinInstances == *clusterConfig.MaxInstances {
		workerInstanceStr = fmt.Sprintf("%d %s %s for your apis", *clusterConfig.MinInstances, *clusterConfig.InstanceType, instanceStr)
		ebsInstanceStr = fmt.Sprintf("%d %dgb ebs %s for your apis", *clusterConfig.MinInstances, clusterConfig.InstanceVolumeSize, volumeStr)
	}

	workerPriceStr := s.DollarsMaxPrecision(apiInstancePrice) + " each"
	isSpot := clusterConfig.Spot != nil && *clusterConfig.Spot
	if isSpot {
		spotPrice, err := awsClient.SpotInstancePrice(*clusterConfig.Region, *clusterConfig.InstanceType)
		workerPriceStr += " (spot pricing unavailable)"
		if err == nil && spotPrice != 0 {
			workerPriceStr = fmt.Sprintf("%s - %s each (varies based on spot price)", s.DollarsMaxPrecision(spotPrice), s.DollarsMaxPrecision(apiInstancePrice))
			totalMinPrice = fixedPrice + float64(*clusterConfig.MinInstances)*(spotPrice+apiEBSPrice)
		}
	}

	rows = append(rows, []interface{}{workerInstanceStr, workerPriceStr})
	rows = append(rows, []interface{}{ebsInstanceStr, s.DollarsAndTenthsOfCents(apiEBSPrice) + " each"})
	rows = append(rows, []interface{}{"1 t3.medium instance for the operator", s.DollarsMaxPrecision(operatorInstancePrice)})
	rows = append(rows, []interface{}{"1 20gb ebs volume for the operator", s.DollarsAndTenthsOfCents(operatorEBSPrice)})
	rows = append(rows, []interface{}{"2 elastic load balancers", s.DollarsMaxPrecision(elbPrice) + " each"})

	items := table.Table{
		Headers: headers,
		Rows:    rows,
	}
	fmt.Println(items.MustFormat(&table.Opts{Sort: pointer.Bool(false)}))

	suffix := ""
	priceStr := s.DollarsAndCents(totalMaxPrice)

	if totalMinPrice != totalMaxPrice {
		priceStr = fmt.Sprintf("%s - %s", s.DollarsAndCents(totalMinPrice), s.DollarsAndCents(totalMaxPrice))
		if isSpot && *clusterConfig.MinInstances != *clusterConfig.MaxInstances {
			suffix = " based on cluster size and spot pricing"
		} else if isSpot && *clusterConfig.MinInstances == *clusterConfig.MaxInstances {
			suffix = " based on spot pricing"
		} else if !isSpot && *clusterConfig.MinInstances != *clusterConfig.MaxInstances {
			suffix = " based on cluster size"
		}
	}

	fmt.Printf("your cluster will cost %s per hour%s\n\n", priceStr, suffix)
	fmt.Printf("cortex will also create an s3 bucket (%s) and a cloudwatch log group (%s)\n\n", clusterConfig.Bucket, clusterConfig.LogGroup)
	fmt.Printf("your cli environment named \"%s\" will be configured to connect to this cluster\n\n", envName)

	if isSpot && clusterConfig.SpotConfig.OnDemandBackup != nil && !*clusterConfig.SpotConfig.OnDemandBackup {
		if *clusterConfig.SpotConfig.OnDemandBaseCapacity == 0 && *clusterConfig.SpotConfig.OnDemandPercentageAboveBaseCapacity == 0 {
			fmt.Printf("warning: you've disabled on-demand instances (%s=0 and %s=0); spot instances are not guaranteed to be available so please take that into account for production clusters; see https://cortex.dev/v/%s/cluster-management/spot-instances for more information\n", clusterconfig.OnDemandBaseCapacityKey, clusterconfig.OnDemandPercentageAboveBaseCapacityKey, consts.CortexVersionMinor)
		} else {
			fmt.Printf("warning: you've enabled spot instances; spot instances are not guaranteed to be available so please take that into account for production clusters; see https://cortex.dev/v/%s/cluster-management/spot-instances for more information\n", consts.CortexVersionMinor)
		}
		fmt.Println()
	}

	if !disallowPrompt {
		exitMessage := fmt.Sprintf("cluster configuration can be modified via the cluster config file; see https://cortex.dev/v/%s/cluster-management/config for more information", consts.CortexVersionMinor)
		prompt.YesOrExit("would you like to continue?", "", exitMessage)
	}
}

func confirmUpdateClusterConfig(clusterConfig clusterconfig.Config, awsCreds AWSCredentials, awsClient *aws.Client, disallowPrompt bool) {
	fmt.Println(clusterConfigConfirmaionStr(clusterConfig, awsCreds, awsClient))

	if !disallowPrompt {
		exitMessage := fmt.Sprintf("cluster configuration can be modified via the cluster config file; see https://cortex.dev/v/%s/cluster-management/config for more information", consts.CortexVersionMinor)
		prompt.YesOrExit(fmt.Sprintf("your cluster (%s in %s) will be updated according to the configuration above, are you sure you want to continue?", clusterConfig.ClusterName, *clusterConfig.Region), "", exitMessage)
	}
}

func clusterConfigConfirmaionStr(clusterConfig clusterconfig.Config, awsCreds AWSCredentials, awsClient *aws.Client) string {
	defaultConfig, _ := clusterconfig.GetDefaults()

	var items table.KeyValuePairs

	items.Add("aws access key id", s.MaskString(awsCreds.AWSAccessKeyID, 4))
	if awsCreds.CortexAWSAccessKeyID != awsCreds.AWSAccessKeyID {
		items.Add("aws access key id", s.MaskString(awsCreds.CortexAWSAccessKeyID, 4)+" (cortex)")
	}
	items.Add(clusterconfig.RegionUserKey, clusterConfig.Region)
	if len(clusterConfig.AvailabilityZones) > 0 {
		items.Add(clusterconfig.AvailabilityZonesUserKey, clusterConfig.AvailabilityZones)
	}
	items.Add(clusterconfig.BucketUserKey, clusterConfig.Bucket)
	items.Add(clusterconfig.ClusterNameUserKey, clusterConfig.ClusterName)
	if clusterConfig.LogGroup != defaultConfig.LogGroup {
		items.Add(clusterconfig.LogGroupUserKey, clusterConfig.LogGroup)
	}

	items.Add(clusterconfig.InstanceTypeUserKey, *clusterConfig.InstanceType)
	items.Add(clusterconfig.MinInstancesUserKey, *clusterConfig.MinInstances)
	items.Add(clusterconfig.MaxInstancesUserKey, *clusterConfig.MaxInstances)
	if clusterConfig.InstanceVolumeSize != defaultConfig.InstanceVolumeSize {
		items.Add(clusterconfig.InstanceVolumeSizeUserKey, clusterConfig.InstanceVolumeSize)
	}
	if clusterConfig.InstanceVolumeType != defaultConfig.InstanceVolumeType {
		items.Add(clusterconfig.InstanceVolumeTypeUserKey, clusterConfig.InstanceVolumeType)
	}
	if clusterConfig.InstanceVolumeIOPS != nil {
		items.Add(clusterconfig.InstanceVolumeIOPSUserKey, *clusterConfig.InstanceVolumeIOPS)
	}

	if clusterConfig.Spot != nil && *clusterConfig.Spot != *defaultConfig.Spot {
		items.Add(clusterconfig.SpotUserKey, s.YesNo(clusterConfig.Spot != nil && *clusterConfig.Spot))

		if clusterConfig.SpotConfig != nil {
			defaultSpotConfig := clusterconfig.SpotConfig{}
			clusterconfig.AutoGenerateSpotConfig(awsClient, &defaultSpotConfig, *clusterConfig.Region, *clusterConfig.InstanceType)

			if !strset.New(clusterConfig.SpotConfig.InstanceDistribution...).IsEqual(strset.New(defaultSpotConfig.InstanceDistribution...)) {
				items.Add(clusterconfig.InstanceDistributionUserKey, clusterConfig.SpotConfig.InstanceDistribution)
			}

			if *clusterConfig.SpotConfig.OnDemandBaseCapacity != *defaultSpotConfig.OnDemandBaseCapacity {
				items.Add(clusterconfig.OnDemandBaseCapacityUserKey, *clusterConfig.SpotConfig.OnDemandBaseCapacity)
			}

			if *clusterConfig.SpotConfig.OnDemandPercentageAboveBaseCapacity != *defaultSpotConfig.OnDemandPercentageAboveBaseCapacity {
				items.Add(clusterconfig.OnDemandPercentageAboveBaseCapacityUserKey, *clusterConfig.SpotConfig.OnDemandPercentageAboveBaseCapacity)
			}

			if *clusterConfig.SpotConfig.MaxPrice != *defaultSpotConfig.MaxPrice {
				items.Add(clusterconfig.MaxPriceUserKey, *clusterConfig.SpotConfig.MaxPrice)
			}

			if *clusterConfig.SpotConfig.InstancePools != *defaultSpotConfig.InstancePools {
				items.Add(clusterconfig.InstancePoolsUserKey, *clusterConfig.SpotConfig.InstancePools)
			}

			if *clusterConfig.SpotConfig.OnDemandBackup != *defaultSpotConfig.OnDemandBackup {
				items.Add(clusterconfig.OnDemandBackupUserKey, s.YesNo(*clusterConfig.SpotConfig.OnDemandBackup))
			}
		}
	}

	if clusterConfig.Telemetry != defaultConfig.Telemetry {
		items.Add(clusterconfig.TelemetryUserKey, clusterConfig.Telemetry)
	}
	if clusterConfig.ImageOperator != defaultConfig.ImageOperator {
		items.Add(clusterconfig.ImageOperatorUserKey, clusterConfig.ImageOperator)
	}
	if clusterConfig.ImageManager != defaultConfig.ImageManager {
		items.Add(clusterconfig.ImageManagerUserKey, clusterConfig.ImageManager)
	}
	if clusterConfig.ImageDownloader != defaultConfig.ImageDownloader {
		items.Add(clusterconfig.ImageDownloaderUserKey, clusterConfig.ImageDownloader)
	}
	if clusterConfig.ImageRequestMonitor != defaultConfig.ImageRequestMonitor {
		items.Add(clusterconfig.ImageRequestMonitorUserKey, clusterConfig.ImageRequestMonitor)
	}
	if clusterConfig.ImageClusterAutoscaler != defaultConfig.ImageClusterAutoscaler {
		items.Add(clusterconfig.ImageClusterAutoscalerUserKey, clusterConfig.ImageClusterAutoscaler)
	}
	if clusterConfig.ImageMetricsServer != defaultConfig.ImageMetricsServer {
		items.Add(clusterconfig.ImageMetricsServerUserKey, clusterConfig.ImageMetricsServer)
	}
	if clusterConfig.ImageNvidia != defaultConfig.ImageNvidia {
		items.Add(clusterconfig.ImageNvidiaUserKey, clusterConfig.ImageNvidia)
	}
	if clusterConfig.ImageFluentd != defaultConfig.ImageFluentd {
		items.Add(clusterconfig.ImageFluentdUserKey, clusterConfig.ImageFluentd)
	}
	if clusterConfig.ImageStatsd != defaultConfig.ImageStatsd {
		items.Add(clusterconfig.ImageStatsdUserKey, clusterConfig.ImageStatsd)
	}
	if clusterConfig.ImageIstioProxy != defaultConfig.ImageIstioProxy {
		items.Add(clusterconfig.ImageIstioProxyUserKey, clusterConfig.ImageIstioProxy)
	}
	if clusterConfig.ImageIstioPilot != defaultConfig.ImageIstioPilot {
		items.Add(clusterconfig.ImageIstioPilotUserKey, clusterConfig.ImageIstioPilot)
	}
	if clusterConfig.ImageIstioCitadel != defaultConfig.ImageIstioCitadel {
		items.Add(clusterconfig.ImageIstioCitadelUserKey, clusterConfig.ImageIstioCitadel)
	}
	if clusterConfig.ImageIstioGalley != defaultConfig.ImageIstioGalley {
		items.Add(clusterconfig.ImageIstioGalleyUserKey, clusterConfig.ImageIstioGalley)
	}

	return items.String()
}
