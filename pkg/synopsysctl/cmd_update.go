/*
Copyright (C) 2019 Synopsys, Inc.

Licensed to the Apache Software Foundation (ASF) under one
or more contributor license agreements. See the NOTICE file
distributed with this work for additional information
regarding copyright ownership. The ASF licenses this file
to you under the Apache License, Version 2.0 (the
"License"); you may not use this file except in compliance
with the License. You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing,
software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
KIND, either express or implied. See the License for the
specific language governing permissions and limitations
under the License.
*/

package synopsysctl

import (
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	horizonapi "github.com/blackducksoftware/horizon/pkg/api"
	"github.com/blackducksoftware/horizon/pkg/components"
	alertctl "github.com/blackducksoftware/synopsysctl/pkg/alert"
	blackduckapi "github.com/blackducksoftware/synopsysctl/pkg/api/blackduck/v1"
	opssightapi "github.com/blackducksoftware/synopsysctl/pkg/api/opssight/v1"
	"github.com/blackducksoftware/synopsysctl/pkg/bdba"
	polarisreporting "github.com/blackducksoftware/synopsysctl/pkg/polaris-reporting"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"

	// bdappsutil "github.com/blackducksoftware/synopsysctl/pkg/apps/util"

	blackduck "github.com/blackducksoftware/synopsysctl/pkg/blackduck"
	opssight "github.com/blackducksoftware/synopsysctl/pkg/opssight"
	"github.com/blackducksoftware/synopsysctl/pkg/polaris"
	"github.com/blackducksoftware/synopsysctl/pkg/util"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	// "k8s.io/apimachinery/pkg/runtime"
)

// Update Command ResourceCtlSpecBuilders
var updateAlertCobraHelper alertctl.HelmValuesFromCobraFlags
var updateBlackDuckCobraHelper blackduck.HelmValuesFromCobraFlags
var updateOpsSightCobraHelper CRSpecBuilderFromCobraFlagsInterface
var updatePolarisCobraHelper polaris.HelmValuesFromCobraFlags
var updatePolarisReportingCobraHelper polarisreporting.HelmValuesFromCobraFlags
var updateBDBACobraHelper bdba.HelmValuesFromCobraFlags

// updateCmd provides functionality to update/upgrade features of
// Synopsys resources
var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update a Synopsys resource",
	RunE: func(cmd *cobra.Command, args []string) error {
		return fmt.Errorf("must specify a sub-command")
	},
}

/*
Update Alert Commands
*/

// updateAlertCmd updates an Alert instance
var updateAlertCmd = &cobra.Command{
	Use:           "alert NAME",
	Example:       "synopsysctl update alert <name>  -n <namespace> --port 80",
	Short:         "Update an Alert instance",
	SilenceUsage:  true,
	SilenceErrors: true,
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) != 1 {
			cmd.Help()
			return fmt.Errorf("this command takes 1 argument but got %+v", args)
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		alertName := args[0]

		// Update the Helm Chart Location
		chartLocationFlag := cmd.Flag("chart-location-path")
		if chartLocationFlag.Changed {
			alertChartRepository = chartLocationFlag.Value.String()
		} else {
			versionFlag := cmd.Flag("version")
			if versionFlag.Changed {
				alertChartRepository = fmt.Sprintf("%s/charts/alert-helmchart-%s.tgz", baseChartRepository, versionFlag.Value.String())
			}
		}

		// TODO verity we can download the chart
		isOperatorBased := false
		instance, err := util.GetWithHelm3(fmt.Sprintf("%s%s", alertName, AlertPostSuffix), namespace, kubeConfigPath)
		if err != nil {
			isOperatorBased = true
		}

		if !isOperatorBased && instance != nil {
			err = updateAlertHelmBased(cmd, fmt.Sprintf("%s%s", alertName, AlertPostSuffix), alertName)
		} else if isOperatorBased {
			versionFlag := cmd.Flag("version")
			if !versionFlag.Changed {
				return fmt.Errorf("you must upgrade this Alert version with --version to use this synopsysctl binary")
			}
			// // TODO: Make sure 6.0.0 is the correct Chart Version for Alert
			// isGreaterThanOrEqualTo, err := util.IsNotDefaultVersionGreaterThanOrEqualTo(versionFlag.Value.String(), 6, 0, 0)
			// if err != nil {
			// 	return fmt.Errorf("failed to compare version: %+v", err)
			// }
			// if !isGreaterThanOrEqualTo {
			// 	return fmt.Errorf("you must upgrade this Alert to version 6.0.0 or after in order to use this synopsysctl binary - you gave version %+v", versionFlag.Value.String())
			// }
			err = updateAlertOperatorBased(cmd, alertName)
		}
		if err != nil {
			return err
		}

		log.Infof("Alert has been successfully Updated in namespace '%s'!", namespace)

		return nil
	},
}

func updateAlertHelmBased(cmd *cobra.Command, alertName string, customerReleaseName string) error {
	// Set flags from the current release in the updateAlertCobraHelper
	helmRelease, err := util.GetWithHelm3(alertName, namespace, kubeConfigPath)
	if err != nil {
		return fmt.Errorf(strings.Replace(fmt.Sprintf("failed to get previous user defined values: %+v", err), fmt.Sprintf("instance '%s' ", alertName), fmt.Sprintf("instance '%s' ", customerReleaseName), 0))
	}
	updateAlertCobraHelper.SetArgs(helmRelease.Config)

	// Update Helm Values with flags
	helmValuesMap, err := updateAlertCobraHelper.GenerateHelmFlagsFromCobraFlags(cmd.Flags())
	if err != nil {
		return err
	}

	// check whether the update Alert version is greater than or equal to 5.0.0
	if cmd.Flag("version").Changed {
		helmValuesMapAlertData := helmValuesMap["alert"].(map[string]interface{})
		oldAlertVersion := helmValuesMapAlertData["imageTag"].(string)
		isGreaterThanOrEqualTo, err := util.IsNotDefaultVersionGreaterThanOrEqualTo(oldAlertVersion, 5, 0, 0)
		if err != nil {
			return fmt.Errorf("failed to check Alert version: %+v", err)
		}

		// if greater than or equal to 5.0.0, then copy PUBLIC_HUB_WEBSERVER_HOST to ALERT_HOSTNAME and PUBLIC_HUB_WEBSERVER_PORT to ALERT_SERVER_PORT
		// and delete PUBLIC_HUB_WEBSERVER_HOST and PUBLIC_HUB_WEBSERVER_PORT from the environs. In future, we need to request the customer to use the new params
		if isGreaterThanOrEqualTo && helmValuesMap["environs"] != nil {
			maps := helmValuesMap["environs"].(map[string]interface{})
			isChanged := false
			if _, ok := maps["PUBLIC_HUB_WEBSERVER_HOST"]; ok {
				if _, ok1 := maps["ALERT_HOSTNAME"]; !ok1 {
					maps["ALERT_HOSTNAME"] = maps["PUBLIC_HUB_WEBSERVER_HOST"]
					isChanged = true
				}
				delete(maps, "PUBLIC_HUB_WEBSERVER_HOST")
			}

			if _, ok := maps["PUBLIC_HUB_WEBSERVER_PORT"]; ok {
				if _, ok1 := maps["ALERT_SERVER_PORT"]; !ok1 {
					maps["ALERT_SERVER_PORT"] = maps["PUBLIC_HUB_WEBSERVER_PORT"]
					isChanged = true
				}
				delete(maps, "PUBLIC_HUB_WEBSERVER_PORT")
			}

			if isChanged {
				util.SetHelmValueInMap(helmValuesMap, []string{"environs"}, maps)
			}
		}
	}

	// Get secrets for Alert
	certificateFlag := cmd.Flag("certificate-file-path")
	certificateKeyFlag := cmd.Flag("certificate-key-file-path")
	if certificateFlag.Changed && certificateKeyFlag.Changed {
		certificateData, err := util.ReadFileData(certificateFlag.Value.String())
		if err != nil {
			log.Fatalf("failed to read certificate file: %+v", err)
		}

		certificateKeyData, err := util.ReadFileData(certificateKeyFlag.Value.String())
		if err != nil {
			log.Fatalf("failed to read certificate file: %+v", err)
		}
		customCertificateSecretName := "alert-custom-certificate"
		customCertificateSecret := alertctl.GetAlertCustomCertificateSecret(namespace, customCertificateSecretName, certificateData, certificateKeyData)
		util.SetHelmValueInMap(helmValuesMap, []string{"webserverCustomCertificatesSecretName"}, customCertificateSecretName)
		if _, err := kubeClient.CoreV1().Secrets(namespace).Create(&customCertificateSecret); err != nil {
			if k8serrors.IsAlreadyExists(err) {
				if _, err := kubeClient.CoreV1().Secrets(namespace).Update(&customCertificateSecret); err != nil {
					return fmt.Errorf("failed to update certificate secret: %+v", err)
				}
			} else {
				return fmt.Errorf("failed to create certificate secret: %+v", err)
			}
		}
	}
	javaKeystoreFlag := cmd.Flag("java-keystore-file-path")
	if javaKeystoreFlag.Changed {
		javaKeystoreData, err := util.ReadFileData(javaKeystoreFlag.Value.String())
		if err != nil {
			log.Fatalf("failed to read Java Keystore file: %+v", err)
		}
		javaKeystoreSecretName := "alert-java-keystore"
		javaKeystoreSecret := alertctl.GetAlertJavaKeystoreSecret(namespace, javaKeystoreSecretName, javaKeystoreData)
		util.SetHelmValueInMap(helmValuesMap, []string{"javaKeystoreSecretName"}, javaKeystoreSecretName)
		if _, err := kubeClient.CoreV1().Secrets(namespace).Create(&javaKeystoreSecret); err != nil {
			if k8serrors.IsAlreadyExists(err) {
				if _, err := kubeClient.CoreV1().Secrets(namespace).Update(&javaKeystoreSecret); err != nil {
					return fmt.Errorf("failed to update javakeystore secret: %+v", err)
				}
			} else {
				return fmt.Errorf("failed to create javakeystore secret: %+v", err)
			}
		}
	}

	// Update Alert Resources
	err = util.UpdateWithHelm3(alertName, namespace, alertChartRepository, helmValuesMap, kubeConfigPath)
	if err != nil {
		return fmt.Errorf("failed to update Alert resources due to %+v", err)
	}
	return nil
}

func updateAlertOperatorBased(cmd *cobra.Command, alertName string) error {
	operatorNamespace := namespace
	isClusterScoped := util.GetClusterScope(apiExtensionClient)
	if isClusterScoped {
		opNamespace, err := util.GetOperatorNamespace(kubeClient, metav1.NamespaceAll)
		if err != nil {
			return err
		}
		if len(opNamespace) > 1 {
			return fmt.Errorf("more than 1 Synopsys Operator found in your cluster")
		}
		operatorNamespace = opNamespace[0]
	}

	crdNamespace := namespace
	if isClusterScoped {
		crdNamespace = metav1.NamespaceAll
	}

	currAlert, err := util.GetAlert(alertClient, crdNamespace, alertName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("error getting Alert '%s' in namespace '%s' due to %+v", alertName, crdNamespace, err)
	}

	if err := migrateAlert(currAlert, operatorNamespace, crdNamespace, cmd.Flags()); err != nil {
		// TODO restart operator if migration failed?
		return err
	}
	return nil
}

// updateBlackDuckCmd updates a Black Duck instance
var updateBlackDuckCmd = &cobra.Command{
	Use:           "blackduck NAME -n NAMESPACE",
	Example:       "synopsyctl update blackduck <name> -n <namespace> --size medium",
	Short:         "Update a Black Duck instance",
	SilenceUsage:  true,
	SilenceErrors: true,
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) != 1 {
			return fmt.Errorf("this command takes 1 argument, but got %+v", args)
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		// Update the Helm Chart Location
		chartLocationFlag := cmd.Flag("chart-location-path")
		if chartLocationFlag.Changed {
			blackduckChartRepository = chartLocationFlag.Value.String()
		} else {
			versionFlag := cmd.Flag("version")
			if versionFlag.Changed {
				blackduckChartRepository = fmt.Sprintf("https://artifactory.internal.synopsys.com/artifactory/bds-hub-helm-snapshot-local/blackduck/blackduck-%s.tgz", versionFlag.Value.String())
			}
		}

		isOperatorBased := false
		instance, err := util.GetWithHelm3(args[0], namespace, kubeConfigPath)
		if err != nil {
			isOperatorBased = true
		}

		if !isOperatorBased && instance != nil {
			updateBlackDuckCobraHelper.SetArgs(instance.Config)
			helmValuesMap, err := updateBlackDuckCobraHelper.GenerateHelmFlagsFromCobraFlags(cmd.Flags())
			if err != nil {
				return err
			}

			secrets, err := blackduck.GetCertsFromFlagsAndSetHelmValue(args[0], namespace, cmd.Flags(), helmValuesMap)
			if err != nil {
				return err
			}
			for _, v := range secrets {
				if secret, err := util.GetSecret(kubeClient, namespace, v.Name); err == nil {
					secret.Data = v.Data
					secret.StringData = v.StringData
					if _, err := util.UpdateSecret(kubeClient, namespace, secret); err != nil {
						return fmt.Errorf("failed to update certificate secret: %+v", err)
					}
				} else {
					if _, err := kubeClient.CoreV1().Secrets(namespace).Create(&v); err != nil {
						return fmt.Errorf("failed to create certificate secret: %+v", err)
					}
				}
			}

			var extraFiles []string
			size, found := instance.Config["size"]
			if found {
				extraFiles = append(extraFiles, fmt.Sprintf("%s.yaml", size.(string)))
			}

			if err := util.UpdateWithHelm3(args[0], namespace, blackduckChartRepository, helmValuesMap, kubeConfigPath, extraFiles...); err != nil {
				return err
			}

			err = blackduck.CRUDServiceOrRoute(restconfig, kubeClient, namespace, args[0], helmValuesMap["exposeui"], helmValuesMap["exposedServiceType"])
			if err != nil {
				return err
			}

		} else if isOperatorBased {
			if !cmd.Flag("version").Changed {
				return fmt.Errorf("you must upgrade this Blackduck version with --version 2020.4.0 and above to use this synopsysctl binary")
			}
			ok, err := util.IsVersionGreaterThanOrEqualTo(cmd.Flag("version").Value.String(), 2020, time.April, 0)
			if err != nil {
				return err
			}

			if !ok {
				return fmt.Errorf("migration is only suported for version 2020.4.0 and above")
			}

			operatorNamespace := namespace
			isClusterScoped := util.GetClusterScope(apiExtensionClient)
			if isClusterScoped {
				opNamespace, err := util.GetOperatorNamespace(kubeClient, metav1.NamespaceAll)
				if err != nil {
					return err
				}
				if len(opNamespace) > 1 {
					return fmt.Errorf("more than 1 Synopsys Operator found in your cluster")
				}
				operatorNamespace = opNamespace[0]
			}

			blackDuckName := args[0]
			crdNamespace := namespace
			if isClusterScoped {
				crdNamespace = metav1.NamespaceAll
			}

			currBlackDuck, err := util.GetBlackduck(blackDuckClient, crdNamespace, blackDuckName, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("error getting Black Duck '%s' in namespace '%s' due to %+v", blackDuckName, crdNamespace, err)
			}
			if err := migrate(currBlackDuck, operatorNamespace, crdNamespace, cmd.Flags()); err != nil {
				return err
			}
		}

		log.Infof("Black Duck has been successfully Updated in namespace '%s'!", namespace)
		return nil
	},
}

// setBlackDuckFileOwnershipJob that sets the Owner of the files
func setBlackDuckFileOwnershipJob(namespace string, name string, pvcName string, ownership int64, wg *sync.WaitGroup) error {
	busyBoxImage := defaultBusyBoxImage
	volumeClaim := components.NewPVCVolume(horizonapi.PVCVolumeConfig{PVCName: pvcName})
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("set-file-ownership-%s", pvcName),
			Namespace: namespace,
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:    "set-file-ownership-container",
							Image:   busyBoxImage,
							Command: []string{"chown", "-R", fmt.Sprintf("%d", ownership), "/setfileownership"},
							VolumeMounts: []corev1.VolumeMount{
								{Name: pvcName, MountPath: "/setfileownership"},
							},
						},
					},
					RestartPolicy: corev1.RestartPolicyNever,
					Volumes: []corev1.Volume{
						{Name: pvcName, VolumeSource: volumeClaim.VolumeSource},
					},
				},
			},
		},
	}
	defer wg.Done()

	job, err := kubeClient.BatchV1().Jobs(namespace).Create(job)
	if err != nil {
		panic(fmt.Sprintf("failed to create job for setting group ownership due to %s", err))
	}

	timeout := time.NewTimer(30 * time.Minute)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	defer timeout.Stop()

	for {
		select {
		case <-timeout.C:
			return fmt.Errorf("failed to set the group ownership of files for PV '%s' in namespace '%s'", pvcName, namespace)

		case <-ticker.C:
			job, err = kubeClient.BatchV1().Jobs(job.Namespace).Get(job.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			if job.Status.Succeeded > 0 {
				log.Infof("successfully set the group ownership of files for PV '%s' in namespace '%s'", pvcName, namespace)
				kubeClient.BatchV1().Jobs(job.Namespace).Delete(job.Name, &metav1.DeleteOptions{})
				return nil
			}
		}
	}
}

// updateBlackDuckMasterKeyCmd create new Black Duck master key for source code upload in the cluster
var updateBlackDuckMasterKeyCmd = &cobra.Command{
	Use:           "masterkey BLACK_DUCK_NAME DIRECTORY_PATH_OF_STORED_MASTER_KEY NEW_SEAL_KEY -n NAMESPACE",
	Example:       "synopsysctl update blackduck masterkey <name> <directory path of the stored master key> <new seal key> -n <namespace>",
	Short:         "Update the master key of the Black Duck instance that is used for source code upload",
	SilenceUsage:  true,
	SilenceErrors: true,
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) != 3 {
			cmd.Help()
			return fmt.Errorf("this command takes 3 arguments, but got %+v", args)
		}

		if len(args[2]) != 32 {
			return fmt.Errorf("new seal key should be of length 32")
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		_, err := util.GetWithHelm3(args[0], namespace, kubeConfigPath)
		if err != nil {
			return fmt.Errorf("couldn't find instance %s in namespace %s", args[0], namespace)
		}
		if err := updateMasterKey(namespace, args[0], args[1], args[2], false); err != nil {
			return err
		}
		return nil
	},
}

// updateBlackDuckMasterKeyNativeCmd create new Black Duck master key for source code upload in the cluster
var updateBlackDuckMasterKeyNativeCmd = &cobra.Command{
	Use:           "native NAME DIRECTORY_PATH_OF_STORED_MASTER_KEY NEW_SEAL_KEY -n NAMESPACE",
	Example:       "synopsysctl update blackduck masterkey native <name> <directory path of the stored master key> <new seal key> -n <namespace>",
	Short:         "Update the master key of the Black Duck instance that is used for source code upload",
	SilenceUsage:  true,
	SilenceErrors: true,
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) != 3 {
			cmd.Help()
			return fmt.Errorf("this command takes 3 arguments, but got %+v", args)
		}

		if len(args[2]) != 32 {
			return fmt.Errorf("new seal key should be of length 32")
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := updateMasterKey(namespace, args[0], args[1], args[2], true); err != nil {
			return err
		}
		return nil
	},
}

// updateMasterKey updates the master key and encoded with new seal key
func updateMasterKey(namespace string, name string, oldMasterKeyFilePath string, newSealKey string, isNative bool) error {

	// getting the seal key secret to retrieve the seal key
	secret, err := util.GetSecret(kubeClient, namespace, fmt.Sprintf("%s-blackduck-upload-cache", name))
	if err != nil {
		return fmt.Errorf("unable to find Seal key secret (%s-blackduck-upload-cache) in namespace '%s' due to %+v", name, namespace, err)
	}

	log.Infof("updating Black Duck '%s's master key in namespace '%s'...", name, namespace)

	// read the old master key
	fileName := filepath.Join(oldMasterKeyFilePath, fmt.Sprintf("%s-%s.key", namespace, name))
	masterKey, err := ioutil.ReadFile(fileName)
	if err != nil {
		return fmt.Errorf("error reading the master key from file '%s' due to %+v", fileName, err)
	}

	// Filter the upload cache pod to get the root key using the seal key
	uploadCachePod, err := util.FilterPodByNamePrefixInNamespace(kubeClient, namespace, util.GetResourceName(name, util.BlackDuckName, "uploadcache"))
	if err != nil {
		return fmt.Errorf("unable to filter the upload cache pod in namespace '%s' due to %+v", namespace, err)
	}

	// Create the exec into Kubernetes pod request
	req := util.CreateExecContainerRequest(kubeClient, uploadCachePod, "/bin/sh")

	_, err = util.ExecContainer(restconfig, req, []string{fmt.Sprintf(`curl -X PUT --header "X-SEAL-KEY:%s" -H "X-MASTER-KEY:%s" https://localhost:9444/api/internal/recovery --cert /opt/blackduck/hub/blackduck-upload-cache/security/blackduck-upload-cache-server.crt --key /opt/blackduck/hub/blackduck-upload-cache/security/blackduck-upload-cache-server.key --cacert /opt/blackduck/hub/blackduck-upload-cache/security/root.crt`, base64.StdEncoding.EncodeToString([]byte(newSealKey)), masterKey)})
	if err != nil {
		return fmt.Errorf("unable to exec into upload cache pod in namespace '%s' due to %+v", namespace, err)
	}

	log.Infof("successfully updated the master key in the upload cache container of Black Duck '%s' in namespace '%s'", name, namespace)

	if isNative {
		// update the new seal key
		secret.Data["SEAL_KEY"] = []byte(newSealKey)
		_, err = util.UpdateSecret(kubeClient, namespace, secret)
		if err != nil {
			return fmt.Errorf("unable to update Seal key secret (%s-blackduck-upload-cache) in namespace '%s' due to %+v", name, namespace, err)
		}

		log.Infof("successfully updated the seal key secret for Black Duck '%s' in namespace '%s'", name, namespace)

		// delete the upload cache pod
		err = util.DeletePod(kubeClient, namespace, uploadCachePod.Name)
		if err != nil {
			return fmt.Errorf("unable to delete an upload cache pod in namespace '%s' due to %+v", namespace, err)
		}

		log.Infof("successfully deleted an upload cache pod for Black Duck '%s' in namespace '%s' to reflect the new seal key. Wait for upload cache pod to restart to resume the source code upload", name, namespace)
	} else {
		helmValuesMap := make(map[string]interface{})
		util.SetHelmValueInMap(helmValuesMap, []string{"sealKey"}, newSealKey)
		if err := util.UpdateWithHelm3(name, namespace, blackduckChartRepository, helmValuesMap, kubeConfigPath); err != nil {
			return err
		}

		log.Infof("successfully submitted updates to Black Duck '%s' in namespace '%s'. Wait for upload cache pod to restart to resume the source code upload", name, namespace)
	}
	return nil
}

// updateBlackDuckAddEnvironCmd adds an Environment Variable to a Black Duck instance
var updateBlackDuckAddEnvironCmd = &cobra.Command{
	Use:           "addenviron NAME (ENVIRON_NAME:ENVIRON_VALUE) -n NAMESPACE",
	Example:       "synopsysctl update blackduck addenviron <name> USE_ALERT:1 -n <namespace>",
	Short:         "Add an Environment Variable to a Black Duck instance",
	SilenceUsage:  true,
	SilenceErrors: true,
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) != 2 {
			cmd.Help()
			return fmt.Errorf("this command takes 2 arguments, but got %+v", args)
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		_, err := util.GetWithHelm3(args[0], namespace, kubeConfigPath)
		if err != nil {
			return fmt.Errorf("couldn't find instance %s in namespace %s", args[0], namespace)
		}

		vals := strings.Split(args[1], ":")
		if len(vals) != 2 {
			return fmt.Errorf("%s is not valid - expecting NAME:VALUE", args[0])
		}
		log.Infof("updating Black Duck '%s' with environ '%s' in namespace '%s'...", args[0], args[1], namespace)

		helmValuesMap := make(map[string]interface{})
		util.SetHelmValueInMap(helmValuesMap, []string{"environs", vals[0]}, vals[1])

		if err := util.UpdateWithHelm3(args[0], namespace, blackduckChartRepository, helmValuesMap, kubeConfigPath); err != nil {
			return err
		}

		log.Infof("successfully submitted updates to Black Duck '%s' in namespace '%s'", args[0], namespace)
		return nil
	},
}

func updateBlackDuckSetImageRegistry(bd *blackduckapi.Blackduck, imageRegistry string) (*blackduckapi.Blackduck, error) {
	// Get the name of the container
	baseContainerName, err := util.GetImageName(imageRegistry)
	if err != nil {
		return nil, err
	}
	// Add Registry to Spec
	var found bool
	for i, imageReg := range bd.Spec.ImageRegistries {
		existingBaseContainerName, err := util.GetImageName(imageReg)
		if err != nil {
			return nil, err
		}
		found = strings.EqualFold(existingBaseContainerName, baseContainerName)
		if found {
			bd.Spec.ImageRegistries[i] = imageRegistry // replace existing imageReg
			break
		}
	}
	if !found { // if didn't already exist, add new imageReg
		bd.Spec.ImageRegistries = append(bd.Spec.ImageRegistries, imageRegistry)
	}
	return bd, nil
}

/*
Update OpsSight Commands
*/

func updateOpsSight(ops *opssightapi.OpsSight, flagset *pflag.FlagSet) (*opssightapi.OpsSight, error) {
	updateOpsSightCobraHelper.SetCRSpec(ops.Spec)
	opsSightInterface, err := updateOpsSightCobraHelper.GenerateCRSpecFromFlags(flagset)
	if err != nil {
		return nil, err
	}
	newSpec := opsSightInterface.(opssightapi.OpsSightSpec)
	ops.Spec = newSpec
	return ops, nil
}

// updateOpsSightCmd updates an OpsSight instance
var updateOpsSightCmd = &cobra.Command{
	Use:           "opssight NAME",
	Example:       "synopsyctl update opssight <name> --blackduck-max-count 2\nsynopsyctl update opssight <name> --blackduck-max-count 2 -n <namespace>",
	Short:         "Update an OpsSight instance",
	SilenceUsage:  true,
	SilenceErrors: true,
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) != 1 {
			cmd.Help()
			return fmt.Errorf("this command takes 1 argument")
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		opsSightName := args[0]
		opsSightNamespace, crdnamespace, _, err := getInstanceInfo(util.OpsSightCRDName, util.OpsSightName, namespace, opsSightName)
		if err != nil {
			return err
		}
		currOpsSight, err := util.GetOpsSight(opsSightClient, crdnamespace, opsSightName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("error getting OpsSight '%s' in namespace '%s' due to %+v", opsSightName, opsSightNamespace, err)
		}
		newOpsSight, err := updateOpsSight(currOpsSight, cmd.Flags())
		if err != nil {
			return err
		}

		// update the namespace label if the version of the app got changed
		// TODO: when opssight versioning PR is merged, the hard coded 2.2.5 version to be replaced with OpsSight
		_, err = util.CheckAndUpdateNamespace(kubeClient, util.OpsSightName, opsSightNamespace, opsSightName, "2.2.5", false)
		if err != nil {
			return err
		}

		log.Infof("updating OpsSight '%s' in namespace '%s'...", opsSightName, opsSightNamespace)
		_, err = util.UpdateOpsSight(opsSightClient, crdnamespace, newOpsSight)
		if err != nil {
			return fmt.Errorf("error updating OpsSight '%s' due to %+v", newOpsSight.Name, err)
		}
		log.Infof("successfully submitted updates to OpsSight '%s' in namespace '%s'", opsSightName, opsSightNamespace)
		return nil
	},
}

func updateOpsSightExternalHost(ops *opssightapi.OpsSight, scheme, domain, port, user, pass, scanLimit string) (*opssightapi.OpsSight, error) {
	hostPort, err := strconv.ParseInt(port, 0, 64)
	if err != nil {
		return nil, err
	}
	hostScanLimit, err := strconv.ParseInt(scanLimit, 0, 64)
	if err != nil {
		return nil, err
	}
	newHost := opssightapi.Host{
		Scheme:              scheme,
		Domain:              domain,
		Port:                int(hostPort),
		User:                user,
		Password:            pass,
		ConcurrentScanLimit: int(hostScanLimit),
	}
	ops.Spec.Blackduck.ExternalHosts = append(ops.Spec.Blackduck.ExternalHosts, &newHost)
	return ops, nil
}

// updateOpsSightExternalHostCmd updates an external host for an OpsSight intance's component
var updateOpsSightExternalHostCmd = &cobra.Command{
	Use:           "externalhost NAME SCHEME DOMAIN PORT USER PASSWORD SCANLIMIT",
	Example:       "synopsysctl update opssight externalhost <name> scheme domain 80 user pass 50\nsynopsysctl update opssight externalhost <name> scheme domain 80 user pass 50 -n <namespace>",
	Short:         "Update an external host for an OpsSight intance's component",
	SilenceUsage:  true,
	SilenceErrors: true,
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) != 7 {
			cmd.Help()
			return fmt.Errorf("this command takes 7 arguments")
		}
		// Check Host Port
		_, err := strconv.ParseInt(args[3], 0, 64)
		if err != nil {
			return fmt.Errorf("invalid port number: '%s'", err)
		}
		// Check Host Scan Limit
		_, err = strconv.ParseInt(args[6], 0, 64)
		if err != nil {
			return fmt.Errorf("invalid concurrent scan limit: %s", err)
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		opsSightName := args[0]
		opsSightNamespace, crdnamespace, _, err := getInstanceInfo(util.OpsSightCRDName, util.OpsSightName, namespace, opsSightName)
		if err != nil {
			return err
		}
		currOpsSight, err := util.GetOpsSight(opsSightClient, crdnamespace, opsSightName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("error getting OpsSight '%s' in namespace '%s' due to %+v", opsSightName, opsSightNamespace, err)
		}
		newOpsSight, err := updateOpsSightExternalHost(currOpsSight, args[1], args[2], args[3], args[4], args[5], args[6])
		if err != nil {
			return err
		}

		log.Infof("updating OpsSight '%s' with an external host in namespace '%s'...", opsSightName, opsSightNamespace)
		_, err = util.UpdateOpsSight(opsSightClient, crdnamespace, newOpsSight)
		if err != nil {
			return fmt.Errorf("error updating OpsSight '%s' due to %+v", newOpsSight.Name, err)
		}
		log.Infof("successfully submitted updates to OpsSight '%s' in namespace '%s'", opsSightName, opsSightNamespace)
		return nil
	},
}

// updateOpsSightExternalHostNativeCmd prints the Kubernetes resources with updates to an external host for an OpsSight intance's component
var updateOpsSightExternalHostNativeCmd = &cobra.Command{
	Use:           "externalhost NAME SCHEME DOMAIN PORT USER PASSWORD SCANLIMIT",
	Example:       "synopsysctl update opssight externalhost native <name> scheme domain 80 user pass 50\nsynopsysctl update opssight externalhost native <name> scheme domain 80 user pass 50 -n <namespace>",
	Short:         "Print the Kubernetes resources with updates to an external host for an OpsSight intance's component",
	SilenceUsage:  true,
	SilenceErrors: true,
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) != 7 {
			cmd.Help()
			return fmt.Errorf("this command takes 7 arguments")
		}
		// Check Host Port
		_, err := strconv.ParseInt(args[3], 0, 64)
		if err != nil {
			return fmt.Errorf("invalid port number: '%s'", err)
		}
		// Check Host Scan Limit
		_, err = strconv.ParseInt(args[6], 0, 64)
		if err != nil {
			return fmt.Errorf("invalid concurrent scan limit: %s", err)
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		opsSightName := args[0]
		opsSightNamespace, crdnamespace, _, err := getInstanceInfo(util.OpsSightCRDName, util.OpsSightName, namespace, opsSightName)
		if err != nil {
			return err
		}
		currOpsSight, err := util.GetOpsSight(opsSightClient, crdnamespace, opsSightName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("error getting OpsSight '%s' in namespace '%s' due to %+v", opsSightName, opsSightNamespace, err)
		}
		newOpsSight, err := updateOpsSightExternalHost(currOpsSight, args[1], args[2], args[3], args[4], args[5], args[6])
		if err != nil {
			return err
		}

		log.Debugf("generating updates to the Kubernetes resources for OpsSight '%s' in namespace '%s'...", opsSightName, opsSightNamespace)
		return PrintResource(*newOpsSight, nativeFormat, true)
	},
}

func updateOpsSightAddRegistry(ops *opssightapi.OpsSight, url, user, pass string) (*opssightapi.OpsSight, error) {
	newReg := opssightapi.RegistryAuth{
		URL:      url,
		User:     user,
		Password: pass,
	}
	ops.Spec.ScannerPod.ImageFacade.InternalRegistries = append(ops.Spec.ScannerPod.ImageFacade.InternalRegistries, &newReg)
	return ops, nil
}

// updateOpsSightAddRegistryCmd adds an internal registry to an OpsSight instance's ImageFacade
var updateOpsSightAddRegistryCmd = &cobra.Command{
	Use:           "registry NAME URL USER PASSWORD",
	Example:       "synopsysctl update opssight registry <name> reg_url reg_username reg_password\nsynopsysctl update opssight registry <name> reg_url reg_username reg_password -n <namespace>",
	Short:         "Add an internal registry to an OpsSight instance's ImageFacade",
	SilenceUsage:  true,
	SilenceErrors: true,
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) != 4 {
			cmd.Help()
			return fmt.Errorf("this command takes 4 arguments")
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		opsSightName := args[0]
		opsSightNamespace, crdnamespace, _, err := getInstanceInfo(util.OpsSightCRDName, util.OpsSightName, namespace, opsSightName)
		if err != nil {
			return err
		}
		currOpsSight, err := util.GetOpsSight(opsSightClient, crdnamespace, opsSightName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("error getting OpsSight '%s' in namespace '%s' due to %+v", opsSightName, opsSightNamespace, err)
		}
		newOpsSight, err := updateOpsSightAddRegistry(currOpsSight, args[1], args[2], args[3])
		if err != nil {
			return err
		}

		log.Infof("updating OpsSight '%s' with internal registry in namespace '%s'...", opsSightName, opsSightNamespace)
		_, err = util.UpdateOpsSight(opsSightClient, crdnamespace, newOpsSight)
		if err != nil {
			return fmt.Errorf("error updating OpsSight '%s' due to %+v", newOpsSight.Name, err)
		}
		log.Infof("successfully submitted updates to OpsSight '%s' in namespace '%s'", opsSightName, opsSightNamespace)
		return nil
	},
}

// updateOpsSightAddRegistryNativeCmd prints the Kubernetes resources with updates from adding an internal registry to an OpsSight instance's ImageFacade
var updateOpsSightAddRegistryNativeCmd = &cobra.Command{
	Use:           "native NAME URL USER PASSWORD",
	Example:       "synopsysctl update opssight registry native <name> reg_url reg_username reg_password\nsynopsysctl update opssight registry native <name> reg_url reg_username reg_password -n <namespace>",
	Short:         "Print the Kubernetes resources with updates from adding an internal registry to an OpsSight instance's ImageFacade",
	SilenceUsage:  true,
	SilenceErrors: true,
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) != 4 {
			cmd.Help()
			return fmt.Errorf("this command takes 4 arguments")
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		opsSightName := args[0]
		opsSightNamespace, crdnamespace, _, err := getInstanceInfo(util.OpsSightCRDName, util.OpsSightName, namespace, opsSightName)
		if err != nil {
			return err
		}
		currOpsSight, err := util.GetOpsSight(opsSightClient, crdnamespace, opsSightName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("error getting OpsSight '%s' in namespace '%s' due to %+v", opsSightName, opsSightNamespace, err)
		}
		newOpsSight, err := updateOpsSightAddRegistry(currOpsSight, args[1], args[2], args[3])
		if err != nil {
			return err
		}

		log.Debugf("generating updates to the Kubernetes resources for OpsSight '%s' in namespace '%s'...", opsSightName, opsSightNamespace)
		return PrintResource(*newOpsSight, nativeFormat, true)
	},
}

// updatePolarisCmd updates a Polaris instance
var updatePolarisCmd = &cobra.Command{
	Use:           "polaris -n NAMESPACE",
	Example:       "synopsyctl update polaris -n <namespace>",
	Short:         "Update a Polaris instance. (Please make sure you have read and understand prerequisites before installing Polaris: https://sig-confluence.internal.synopsys.com/display/DD/Polaris+on-premises])",
	SilenceUsage:  true,
	SilenceErrors: true,
	Args: func(cmd *cobra.Command, args []string) error {
		// Check the Number of Arguments
		if len(args) != 0 {
			cmd.Help()
			return fmt.Errorf("this command takes 0 arguments, but got %+v", args)
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		helmRelease, err := util.GetWithHelm3(polarisName, namespace, kubeConfigPath)
		if err != nil {
			return fmt.Errorf("failed to get previous user defined values: %+v", err)
		}
		updatePolarisCobraHelper.SetArgs(helmRelease.Config)
		// Get the flags to set Helm values
		helmValuesMap, err := updatePolarisCobraHelper.GenerateHelmFlagsFromCobraFlags(cmd.Flags())
		if err != nil {
			return err
		}

		// Update the Helm Chart Location
		chartLocationFlag := cmd.Flag("chart-location-path")
		if chartLocationFlag.Changed {
			polarisChartRepository = chartLocationFlag.Value.String()
		} else {
			versionFlag := cmd.Flag("version")
			if versionFlag.Changed {
				polarisChartRepository = fmt.Sprintf("%s/charts/polaris-helmchart-%s.tgz", baseChartRepository, versionFlag.Value.String())
			}
		}

		// Deploy Polaris Resources
		err = util.UpdateWithHelm3(polarisName, namespace, polarisChartRepository, helmValuesMap, kubeConfigPath)
		if err != nil {
			return fmt.Errorf("failed to update Polaris resources due to %+v", err)
		}

		log.Infof("Polaris has been successfully Updated in namespace '%s'!", namespace)
		return nil
	},
}

// updatePolarisReportingCmd updates a Polaris-Reporting instance
var updatePolarisReportingCmd = &cobra.Command{
	Use:           "polaris-reporting -n NAMESPACE",
	Example:       "synopsysctl update polaris-reporting -n <namespace>",
	Short:         "Update a Polaris-Reporting instance",
	SilenceUsage:  true,
	SilenceErrors: true,
	Args: func(cmd *cobra.Command, args []string) error {
		// Check the Number of Arguments
		if len(args) != 0 {
			cmd.Help()
			return fmt.Errorf("this command takes 0 argument, but got %+v", args)
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		helmRelease, err := util.GetWithHelm3(polarisReportingName, namespace, kubeConfigPath)
		if err != nil {
			return fmt.Errorf("failed to get previous user defined values: %+v", err)
		}
		updatePolarisReportingCobraHelper.SetArgs(helmRelease.Config)

		// Get the flags to set Helm values
		helmValuesMap, err := updatePolarisReportingCobraHelper.GenerateHelmFlagsFromCobraFlags(cmd.Flags())
		if err != nil {
			return err
		}

		// Update the Helm Chart Location
		chartLocationFlag := cmd.Flag("chart-location-path")
		if chartLocationFlag.Changed {
			polarisReportingChartRepository = chartLocationFlag.Value.String()
		} else {
			versionFlag := cmd.Flag("version")
			if versionFlag.Changed {
				polarisReportingChartRepository = fmt.Sprintf("%s/charts/polaris-helmchart-reporting-%s.tgz", baseChartRepository, versionFlag.Value.String())
			}
		}

		// Update Polaris-Reporting Resources
		err = util.UpdateWithHelm3(polarisReportingName, namespace, polarisReportingChartRepository, helmValuesMap, kubeConfigPath)
		if err != nil {
			return fmt.Errorf("failed to update Polaris-Reporting resources due to %+v", err)
		}

		log.Infof("Polaris-Reporting has been successfully Updated in namespace '%s'!", namespace)
		return nil
	},
}

// updateBDBACmd updates a BDBA instance
var updateBDBACmd = &cobra.Command{
	Use:           "bdba -n NAMESPACE",
	Example:       "synopsysctl update bdba -n <namespace>",
	Short:         "Update a BDBA instance",
	SilenceUsage:  true,
	SilenceErrors: true,
	Args: func(cmd *cobra.Command, args []string) error {
		// Check the Number of Arguments
		if len(args) != 0 {
			cmd.Help()
			return fmt.Errorf("this command takes 0 arguments, but got %+v", args)
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		helmRelease, err := util.GetWithHelm3(bdbaName, namespace, kubeConfigPath)
		if err != nil {
			return fmt.Errorf("failed to get previous user defined values: %+v", err)
		}
		updateBDBACobraHelper.SetArgs(helmRelease.Config)

		// Get the flags to set Helm values
		helmValuesMap, err := updateBDBACobraHelper.GenerateHelmFlagsFromCobraFlags(cmd.Flags())
		if err != nil {
			return err
		}

		// Update the Helm Chart Location
		chartLocationFlag := cmd.Flag("chart-location-path")
		if chartLocationFlag.Changed {
			bdbaChartRepository = chartLocationFlag.Value.String()
		} else {
			versionFlag := cmd.Flag("version")
			if versionFlag.Changed {
				bdbaChartRepository = fmt.Sprintf("%s/charts/bdba-%s.tgz", baseChartRepository, versionFlag.Value.String())
			}
		}

		// Update Resources
		err = util.UpdateWithHelm3(bdbaName, namespace, bdbaChartRepository, helmValuesMap, kubeConfigPath)
		if err != nil {
			return fmt.Errorf("failed to update BDBA resources due to %+v", err)
		}

		log.Infof("BDBA has been successfully Updated in namespace '%s'!", namespace)
		return nil
	},
}

func init() {
	// initialize global resource ctl structs for commands to use
	updateBlackDuckCobraHelper = *blackduck.NewHelmValuesFromCobraFlags()
	updateOpsSightCobraHelper = opssight.NewCRSpecBuilderFromCobraFlags()
	updateAlertCobraHelper = *alertctl.NewHelmValuesFromCobraFlags()
	updatePolarisCobraHelper = *polaris.NewHelmValuesFromCobraFlags()
	updatePolarisReportingCobraHelper = *polarisreporting.NewHelmValuesFromCobraFlags()
	updateBDBACobraHelper = *bdba.NewHelmValuesFromCobraFlags()

	rootCmd.AddCommand(updateCmd)

	// updateAlertCmd
	updateAlertCmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", namespace, "Namespace of the instance(s)")
	cobra.MarkFlagRequired(updateAlertCmd.PersistentFlags(), "namespace")
	updateAlertCobraHelper.AddCobraFlagsToCommand(updateAlertCmd, false)
	addChartLocationPathFlag(updateAlertCmd)
	updateCmd.AddCommand(updateAlertCmd)

	/* Update Black Duck Comamnds */

	// updateBlackDuckCmd
	updateBlackDuckCmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", namespace, "Namespace of the instance(s)")
	cobra.MarkFlagRequired(updateBlackDuckCmd.PersistentFlags(), "namespace")
	addChartLocationPathFlag(updateBlackDuckCmd)
	updateBlackDuckCobraHelper.AddCRSpecFlagsToCommand(updateBlackDuckCmd, false)
	updateCmd.AddCommand(updateBlackDuckCmd)

	// updateBlackDuckMasterKeyCmd
	updateBlackDuckCmd.AddCommand(updateBlackDuckMasterKeyCmd)

	// updateBlackDuckMasterKeyNativeCmd
	updateBlackDuckMasterKeyCmd.AddCommand(updateBlackDuckMasterKeyNativeCmd)

	// updateBlackDuckAddEnvironCmd
	updateBlackDuckCmd.AddCommand(updateBlackDuckAddEnvironCmd)

	/* Update OpsSight Comamnds */

	// updateOpsSightCmd
	updateOpsSightCmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", namespace, "Namespace of the instance(s)")
	updateOpsSightCobraHelper.AddCRSpecFlagsToCommand(updateOpsSightCmd, false)
	updateCmd.AddCommand(updateOpsSightCmd)

	// updateOpsSightExternalHostCmd
	updateOpsSightCmd.AddCommand(updateOpsSightExternalHostCmd)

	addNativeFormatFlag(updateOpsSightExternalHostNativeCmd)
	updateOpsSightExternalHostCmd.AddCommand(updateOpsSightExternalHostNativeCmd)

	// updateOpsSightAddRegistryCmd
	updateOpsSightCmd.AddCommand(updateOpsSightAddRegistryCmd)

	addNativeFormatFlag(updateOpsSightAddRegistryNativeCmd)
	updateOpsSightAddRegistryCmd.AddCommand(updateOpsSightAddRegistryNativeCmd)

	// Polaris
	updatePolarisCmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", namespace, "Namespace of the instance(s)")
	cobra.MarkFlagRequired(updatePolarisCmd.PersistentFlags(), "namespace")
	updatePolarisCobraHelper.AddCobraFlagsToCommand(updatePolarisCmd, false)
	addChartLocationPathFlag(updatePolarisCmd)
	updateCmd.AddCommand(updatePolarisCmd)

	// Polaris-Reporting
	updatePolarisReportingCmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", namespace, "Namespace of the instance(s)")
	cobra.MarkFlagRequired(updatePolarisReportingCmd.PersistentFlags(), "namespace")
	updatePolarisReportingCobraHelper.AddCobraFlagsToCommand(updatePolarisReportingCmd, false)
	addChartLocationPathFlag(updatePolarisReportingCmd)
	updateCmd.AddCommand(updatePolarisReportingCmd)

	// BDBA
	updateBDBACmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", namespace, "Namespace of the instance(s)")
	cobra.MarkFlagRequired(updateBDBACmd.PersistentFlags(), "namespace")
	updateBDBACobraHelper.AddCobraFlagsToCommand(updateBDBACmd, false)
	addChartLocationPathFlag(updateBDBACmd)
	updateCmd.AddCommand(updateBDBACmd)
}
