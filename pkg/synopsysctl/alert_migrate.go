/*
 * Copyright (C) 2019 Synopsys, Inc.
 *
 *  Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 *  with the License. You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 *  under the License.
 */

package synopsysctl

import (
	"fmt"
	"strings"

	alertctl "github.com/blackducksoftware/synopsysctl/pkg/alert"
	v1 "github.com/blackducksoftware/synopsysctl/pkg/api/alert/v1"
	"github.com/blackducksoftware/synopsysctl/pkg/util"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func migrateAlert(alert *v1.Alert, operatorNamespace string, crdNamespace string, flags *pflag.FlagSet) error {
	// TODO ensure operator is installed and running a recent version that doesn't require additional migration

	log.Info("stopping Synopsys Operator")
	soOperatorDeploy, err := util.GetDeployment(kubeClient, operatorNamespace, "synopsys-operator")
	if err != nil {
		return err
	}

	soOperatorDeploy, err = util.PatchDeploymentForReplicas(kubeClient, soOperatorDeploy, util.IntToInt32(0))
	if err != nil {
		return err
	}

	// Generate Helm values for the current CR Instance
	currHelmValuesMap, err := AlertV1ToHelmValues(alert, operatorNamespace)
	if err != nil {
		return err
	}

	// Put helm values into the CobraHelper
	updateAlertCobraHelper.SetArgs(currHelmValuesMap)

	// Get Helm Values if User updated more than just the version
	helmValuesMap, err := updateAlertCobraHelper.GenerateHelmFlagsFromCobraFlags(flags)
	if err != nil {
		return err
	}

	if alert.Spec.PersistentStorage {
		// Set PVC Name to old pvc name format
		pvcList, err := kubeClient.CoreV1().PersistentVolumeClaims(alert.Spec.Namespace).List(metav1.ListOptions{
			LabelSelector: fmt.Sprintf("app=%s, name=%s", "alert", alert.Name),
		})
		if err != nil {
			return err
		}
		if len(pvcList.Items) != 1 {
			return fmt.Errorf("there should be only 1 pvc for alert but got %+v", len(pvcList.Items))
		}
		pvc := pvcList.Items[0]
		pvc.Labels = util.InitLabels(pvc.Labels)
		pvc.Labels["name"] = fmt.Sprintf("%s%s", alert.Name, AlertPostSuffix)
		if _, err = util.UpdatePVC(kubeClient, alert.Spec.Namespace, &pvc); err != nil {
			log.Errorf("unable to update an alert persistent volume claim due to %+v", err)
		}
		util.SetHelmValueInMap(helmValuesMap, []string{"persistentVolumeClaimName"}, pvcList.Items[0].Name)
	}

	log.Info("upgrading Alert instance")

	// Delete the Current Instance's Resources (except PVCs)
	log.Info("cleaning Current Alert resources")
	// TODO wait for resources to be deleted
	// if len(alert.Namespace) == 0 {
	// 	alert.Namespace = alert.Name
	// }
	if err := deleteComponents(alert.Spec.Namespace, alert.Name, util.AlertName); err != nil {
		return err
	}

	// Update the Helm Chart Location
	chartLocationFlag := flags.Lookup("chart-location-path")
	if chartLocationFlag.Changed {
		alertChartRepository = chartLocationFlag.Value.String()
	} else {
		versionFlag := flags.Lookup("version")
		if versionFlag.Changed {
			alertChartRepository = fmt.Sprintf("%s/charts/synopys-alert-%s.tgz", baseChartRepository, versionFlag.Value.String())
		}
	}

	// check whether the update Alert version is greater than or equal to 5.0.0
	if flags.Lookup("version").Changed {
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

	newReleaseName := fmt.Sprintf("%s%s", alert.Name, AlertPostSuffix)

	// Verify Alert can be created with Dry-Run before creating resources
	err = util.CreateWithHelm3(newReleaseName, alert.Spec.Namespace, alertChartRepository, helmValuesMap, kubeConfigPath, true)
	if err != nil {
		return fmt.Errorf("failed to update Alert resources: %+v", err)
	}

	// Update the Secrets
	if len(alert.Spec.Certificate) > 0 && len(alert.Spec.CertificateKey) > 0 {
		customCertificateSecretName := util.GetHelmValueFromMap(helmValuesMap, []string{"webserverCustomCertificatesSecretName"}).(string)
		customCertificateSecret := alertctl.GetAlertCustomCertificateSecret(namespace, customCertificateSecretName, alert.Spec.Certificate, alert.Spec.CertificateKey)
		if secret, err := util.GetSecret(kubeClient, namespace, customCertificateSecretName); err == nil {
			secret.Data = customCertificateSecret.Data
			secret.StringData = customCertificateSecret.StringData
			if _, err := util.UpdateSecret(kubeClient, namespace, secret); err != nil {
				return fmt.Errorf("failed to update certificate secret: %+v", err)
			}
		} else {
			if _, err := kubeClient.CoreV1().Secrets(namespace).Create(&customCertificateSecret); err != nil {
				return fmt.Errorf("failed to create certificate secret: %+v", err)
			}
		}
	}

	if len(alert.Spec.JavaKeyStore) > 0 {
		javaKeystoreSecretName := util.GetHelmValueFromMap(helmValuesMap, []string{"javaKeystoreSecretName"}).(string)
		javaKeystoreSecret := alertctl.GetAlertJavaKeystoreSecret(namespace, javaKeystoreSecretName, alert.Spec.JavaKeyStore)
		if secret, err := util.GetSecret(kubeClient, namespace, javaKeystoreSecretName); err == nil {
			secret.Data = javaKeystoreSecret.Data
			secret.StringData = javaKeystoreSecret.StringData
			if _, err := util.UpdateSecret(kubeClient, namespace, secret); err != nil {
				return fmt.Errorf("failed to update javakeystore secret: %+v", err)
			}
		} else {
			if _, err := kubeClient.CoreV1().Secrets(namespace).Create(&javaKeystoreSecret); err != nil {
				return fmt.Errorf("failed to create javakeystore secret: %+v", err)
			}
		}
	}

	if svc, err := util.GetService(kubeClient, namespace, fmt.Sprintf("%s-exposed", newReleaseName)); err == nil {
		svc.Kind = "Service"
		svc.APIVersion = "v1"
		svc.Labels = util.InitLabels(svc.Labels)
		svc.Labels["name"] = newReleaseName
		svc.Spec.Selector = util.InitLabels(svc.Spec.Selector)
		svc.Spec.Selector["name"] = newReleaseName
		if _, err = util.UpdateService(kubeClient, namespace, svc); err != nil {
			return fmt.Errorf("failed to deploy the alert exposed service: %s", err)
		}
	}

	// Deploy new Resources
	err = util.CreateWithHelm3(newReleaseName, alert.Spec.Namespace, alertChartRepository, helmValuesMap, kubeConfigPath, false)
	if err != nil {
		return fmt.Errorf("failed to update Alert resources: %+v", err)
	}

	log.Info("deleting Alert custom resource")
	if err := util.DeleteAlert(alertClient, alert.Name, alert.Namespace, &metav1.DeleteOptions{}); err != nil {
		return err
	}

	_, err = util.CheckAndUpdateNamespace(kubeClient, util.AlertName, alert.Spec.Namespace, alert.Name, "", true)
	if err != nil {
		log.Warnf("unable to patch the namespace to remove an app labels due to %+v", err)
	}

	return destroyOperator(operatorNamespace, crdNamespace)
}

// AlertV1ToHelmValues converts an Alert v1 Spec to a Helm Values Map
func AlertV1ToHelmValues(alert *v1.Alert, operatorNamespace string) (map[string]interface{}, error) {
	helmValuesMap := make(map[string]interface{})

	if len(alert.Spec.Version) > 0 {
		util.SetHelmValueInMap(helmValuesMap, []string{"alert", "imageTag"}, alert.Spec.Version)
	}

	if len(alert.Spec.ExposeService) > 0 {
		switch alert.Spec.ExposeService {
		case util.NODEPORT:
			util.SetHelmValueInMap(helmValuesMap, []string{"exposedServiceType"}, "NodePort")
			util.SetHelmValueInMap(helmValuesMap, []string{"exposeui"}, false)
		case util.LOADBALANCER:
			util.SetHelmValueInMap(helmValuesMap, []string{"exposedServiceType"}, "LoadBalancer")
			util.SetHelmValueInMap(helmValuesMap, []string{"exposeui"}, false)
		case util.NONE:
			util.SetHelmValueInMap(helmValuesMap, []string{"exposeui"}, false)
		}
	}

	util.SetHelmValueInMap(helmValuesMap, []string{"enableStandalone"}, *alert.Spec.StandAlone)

	if alert.Spec.Port != nil {
		util.SetHelmValueInMap(helmValuesMap, []string{"alert", "port"}, *alert.Spec.Port)
	}

	if len(alert.Spec.EncryptionPassword) > 0 {
		util.SetHelmValueInMap(helmValuesMap, []string{"setEncryptionSecretData"}, true)
		util.SetHelmValueInMap(helmValuesMap, []string{"alertEncryptionPassword"}, alert.Spec.EncryptionPassword)
	}

	if len(alert.Spec.EncryptionGlobalSalt) > 0 {
		util.SetHelmValueInMap(helmValuesMap, []string{"setEncryptionSecretData"}, true)
		util.SetHelmValueInMap(helmValuesMap, []string{"alertEncryptionGlobalSalt"}, alert.Spec.EncryptionGlobalSalt)
	}

	util.SetHelmValueInMap(helmValuesMap, []string{"enablePersistentStorage"}, alert.Spec.PersistentStorage)

	if len(alert.Spec.PVCName) > 0 {
		util.SetHelmValueInMap(helmValuesMap, []string{"persistentVolumeClaimName"}, alert.Spec.PVCName)
	}

	if len(alert.Spec.PVCStorageClass) > 0 {
		util.SetHelmValueInMap(helmValuesMap, []string{"storageClassName"}, alert.Spec.PVCStorageClass)
	}

	if len(alert.Spec.PVCSize) > 0 {
		util.SetHelmValueInMap(helmValuesMap, []string{"pvcSize"}, alert.Spec.PVCSize)
	}

	if len(alert.Spec.AlertMemory) > 0 {
		util.SetHelmValueInMap(helmValuesMap, []string{"alert", "resources", "limits", "memory"}, alert.Spec.AlertMemory)
		util.SetHelmValueInMap(helmValuesMap, []string{"alert", "resources", "requests", "memory"}, alert.Spec.AlertMemory)
	}

	if len(alert.Spec.CfsslMemory) > 0 {
		util.SetHelmValueInMap(helmValuesMap, []string{"cfssl", "resources", "limits", "memory"}, alert.Spec.CfsslMemory)
		util.SetHelmValueInMap(helmValuesMap, []string{"cfssl", "resources", "requests", "memory"}, alert.Spec.CfsslMemory)
	}

	if len(alert.Spec.Environs) > 0 {
		envMap := map[string]interface{}{}
		for _, env := range alert.Spec.Environs {
			envSplit := strings.Split(env, ":")
			envMap[envSplit[0]] = envSplit[1]
		}
		util.SetHelmValueInMap(helmValuesMap, []string{"environs"}, envMap)
	}

	if len(alert.Spec.DesiredState) > 0 {
		if strings.ToUpper(alert.Spec.DesiredState) == "STOPPED" {
			util.SetHelmValueInMap(helmValuesMap, []string{"status"}, "Stopped")
		} else {
			util.SetHelmValueInMap(helmValuesMap, []string{"status"}, "Running")
		}
	}

	if alert.Spec.RegistryConfiguration != nil {
		util.SetHelmValueInMap(helmValuesMap, []string{"registry"}, alert.Spec.RegistryConfiguration.Registry)
		util.SetHelmValueInMap(helmValuesMap, []string{"imagePullSecrets"}, alert.Spec.RegistryConfiguration.PullSecrets)
	}

	if len(alert.Spec.Certificate) > 0 && len(alert.Spec.CertificateKey) > 0 {
		customCertificateSecretName := "alert-custom-certificate"
		util.SetHelmValueInMap(helmValuesMap, []string{"webserverCustomCertificatesSecretName"}, customCertificateSecretName)
	}

	if len(alert.Spec.JavaKeyStore) > 0 {
		javaKeystoreSecretName := "alert-java-keystore"
		util.SetHelmValueInMap(helmValuesMap, []string{"javaKeystoreSecretName"}, javaKeystoreSecretName)
	}

	return helmValuesMap, nil
}
