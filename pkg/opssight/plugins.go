/*
Copyright (C) 2018 Synopsys, Inc.

Licensed to the Apache Software Foundation (ASF) under one
or more contributor license agreements. See the NOTICE file
distributed with this work for additional information
regarding copyright ownershia. The ASF licenses this file
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

package opssight

// This is a controller that deletes the hub based on the delete threshold

import (
	"fmt"
	"strings"
	"time"

	blackduckapi "github.com/blackducksoftware/synopsysctl/pkg/api/blackduck/v1"
	opssightapi "github.com/blackducksoftware/synopsysctl/pkg/api/opssight/v1"
	hubclient "github.com/blackducksoftware/synopsysctl/pkg/blackduck/client/clientset/versioned"
	opssightclientset "github.com/blackducksoftware/synopsysctl/pkg/opssight/client/clientset/versioned"
	"github.com/blackducksoftware/synopsysctl/pkg/protoform"
	"github.com/blackducksoftware/synopsysctl/pkg/util"
	"github.com/juju/errors"
	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

var logger *log.Entry

func init() {
	logger = log.WithField("subsystem", "opssight-plugins")
}

// This is a controller that updates the secret in perceptor periodically.
// It is assumed that the secret in perceptor will roll over any time this is updated, and
// if not, that there is a problem in the orchestration environment.

// Updater stores the opssight updater configuration
type Updater struct {
	config         *protoform.Config
	kubeClient     *kubernetes.Clientset
	hubClient      *hubclient.Clientset
	opssightClient *opssightclientset.Clientset
}

// NewUpdater returns the opssight updater configuration
func NewUpdater(config *protoform.Config, kubeClient *kubernetes.Clientset, hubClient *hubclient.Clientset, opssightClient *opssightclientset.Clientset) *Updater {
	return &Updater{
		config:         config,
		kubeClient:     kubeClient,
		hubClient:      hubClient,
		opssightClient: opssightClient,
	}
}

// Run watches for Black Duck and OpsSight events and update the internal Black Duck hosts in Perceptor secret and
// then patch the corresponding replication controller
func (p *Updater) Run(ch <-chan struct{}) {
	logger.Infof("Starting controller for blackduck<->opssight-core updates... this blocks, so running in a go func.")

	go func() {
		for {
			select {
			case <-ch:
				// stop
				return
			default:
				syncFunc := func() {
					err := p.updateAllHubs()
					if len(err) > 0 {
						logger.Errorf("unable to update Black Ducks because %+v", err)
					}
				}

				// watch for Black Duck events to update an OpsSight internal host only if Black Duck crd is enabled
				if strings.Contains(p.config.CrdNames, util.BlackDuckCRDName) {
					log.Debugf("watch for Black Duck events to update an OpsSight internal hosts")
					syncFunc()

					hubListWatch := &cache.ListWatch{
						ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
							return p.hubClient.SynopsysV1().Blackducks(p.config.CrdNamespace).List(options)
						},
						WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
							return p.hubClient.SynopsysV1().Blackducks(p.config.CrdNamespace).Watch(options)
						},
					}
					_, hubController := cache.NewInformer(hubListWatch,
						&blackduckapi.Blackduck{},
						2*time.Second,
						cache.ResourceEventHandlerFuncs{
							// TODO kinda dumb, we just do a complete re-list of all hubs,
							// every time an event happens... But thats all we need to do, so its good enough.
							DeleteFunc: func(obj interface{}) {
								logger.Debugf("updater - blackduck deleted event ! %v ", obj)
								syncFunc()
							},

							AddFunc: func(obj interface{}) {
								logger.Debugf("updater - blackduck added event! %v ", obj)
								running := p.isBlackDuckRunning(obj)
								if !running {
									syncFunc()
								}
							},
						},
					)

					// make sure this is called from a go func -- it blocks!
					go hubController.Run(ch)
					<-ch
				} else {
					time.Sleep(5 * time.Second)
				}
			}
		}
	}()
}

// isBlackDuckRunning return whether the Black Duck instance is in running state
func (p *Updater) isBlackDuckRunning(obj interface{}) bool {
	blackduck, _ := obj.(*blackduckapi.Blackduck)
	if strings.EqualFold(blackduck.Status.State, "Running") {
		return true
	}
	return false
}

// updateAllHubs will update the Black Duck instances in opssight resources
func (p *Updater) updateAllHubs() []error {
	opssights, err := util.ListOpsSights(p.opssightClient, p.config.CrdNamespace, metav1.ListOptions{})
	if err != nil {
		return []error{errors.Annotatef(err, "unable to list opssight in namespace %s", p.config.Namespace)}
	}

	if len(opssights.Items) == 0 {
		return nil
	}

	errList := []error{}
	for _, opssight := range opssights.Items {
		err = p.updateOpsSight(&opssight)
		if err != nil {
			errList = append(errList, errors.Annotate(err, "unable to update opssight"))
		}
	}
	return errList
}

// updateOpsSight will update the opssight resource with latest Black Duck instances
func (p *Updater) updateOpsSight(opssight *opssightapi.OpsSight) error {
	var err error
	if !strings.EqualFold(opssight.Status.State, "stopped") && !strings.EqualFold(opssight.Status.State, "error") {
		for j := 0; j < 20; j++ {
			if strings.EqualFold(opssight.Status.State, "running") {
				break
			}
			logger.Debugf("waiting for opssight %s to be up.....", opssight.Name)
			time.Sleep(10 * time.Second)

			opssight, err = util.GetOpsSight(p.opssightClient, p.config.CrdNamespace, opssight.Name, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("unable to get opssight %s due to %+v", opssight.Name, err)
			}
		}
		err = p.update(opssight)
	}
	return err
}

// update will list all Black Ducks in the cluster, and send them to opssight as scan targets.
func (p *Updater) update(opssight *opssightapi.OpsSight) error {
	hubType := opssight.Spec.Blackduck.BlackduckSpec.Type

	blackduckPassword, err := util.Base64Decode(opssight.Spec.Blackduck.BlackduckPassword)
	if err != nil {
		return errors.Annotate(err, "unable to decode blackduckPassword")
	}

	allHubs := p.getAllHubs(hubType, blackduckPassword)

	err = p.updateOpsSightCRD(opssight, allHubs)
	if err != nil {
		return errors.Annotate(err, "unable to update opssight CRD")
	}
	return nil
}

// getAllHubs get only the internal Black Duck instances from the cluster
func (p *Updater) getAllHubs(hubType string, blackduckPassword string) []*opssightapi.Host {
	hosts := []*opssightapi.Host{}
	hubsList, err := util.ListBlackduck(p.hubClient, p.config.CrdNamespace, metav1.ListOptions{})
	if err != nil {
		log.Errorf("unable to list blackducks due to %+v", err)
	}
	for _, hub := range hubsList.Items {
		if strings.EqualFold(hub.Spec.Type, hubType) {
			var concurrentScanLimit int
			switch strings.ToUpper(hub.Spec.Size) {
			case "MEDIUM":
				concurrentScanLimit = 3
			case "LARGE":
				concurrentScanLimit = 4
			case "X-LARGE":
				concurrentScanLimit = 6
			default:
				concurrentScanLimit = 2
			}
			host := &opssightapi.Host{
				Domain:              fmt.Sprintf("%s.%s.svc", util.GetResourceName(hub.Name, util.BlackDuckName, "webserver"), hub.Spec.Namespace),
				ConcurrentScanLimit: concurrentScanLimit,
				Scheme:              "https",
				User:                "sysadmin",
				Port:                443,
				Password:            blackduckPassword,
			}
			hosts = append(hosts, host)
		}
	}
	log.Debugf("total no of Black Duck's for type %s is %d", hubType, len(hosts))
	return hosts
}

// updateOpsSightCRD will update the opssight CRD
func (p *Updater) updateOpsSightCRD(opsSight *opssightapi.OpsSight, hubs []*opssightapi.Host) error {
	opssightName := opsSight.Name
	opsSightNamespace := opsSight.Spec.Namespace
	logger.WithField("opssight", opssightName).Info("update opssight: looking for opssight")
	opssight, err := util.GetOpsSight(p.opssightClient, p.config.CrdNamespace, opssightName, metav1.GetOptions{})
	if err != nil {
		return errors.Annotatef(err, "unable to get opssight %s in %s namespace", opssightName, opsSightNamespace)
	}

	opssight.Status.InternalHosts = p.appendBlackDuckHosts(opssight.Status.InternalHosts, hubs)

	_, err = util.UpdateOpsSight(p.opssightClient, p.config.CrdNamespace, opsSight)
	if err != nil {
		return errors.Annotatef(err, "unable to update opssight %s in %s", opssightName, opsSightNamespace)
	}
	return nil
}

// appendBlackDuckHosts will append the old and new internal Black Duck hosts
func (p *Updater) appendBlackDuckHosts(oldBlackDucks []*opssightapi.Host, newBlackDucks []*opssightapi.Host) []*opssightapi.Host {
	existingBlackDucks := make(map[string]*opssightapi.Host)
	for _, oldBlackDuck := range oldBlackDucks {
		existingBlackDucks[oldBlackDuck.Domain] = oldBlackDuck
	}

	finalBlackDucks := []*opssightapi.Host{}
	for _, newBlackDuck := range newBlackDucks {
		if existingBlackduck, ok := existingBlackDucks[newBlackDuck.Domain]; ok {
			// add the existing internal Black Duck from the final Black Duck list
			finalBlackDucks = append(finalBlackDucks, existingBlackduck)
		} else {
			// add the new internal Black Duck to the final Black Duck list
			finalBlackDucks = append(finalBlackDucks, newBlackDuck)
		}
	}

	return finalBlackDucks
}

// appendBlackDuckSecrets will append the secrets of external and internal Black Duck
func (p *Updater) appendBlackDuckSecrets(existingExternalBlackDucks map[string]*opssightapi.Host, oldInternalBlackDucks []*opssightapi.Host, newInternalBlackDucks []*opssightapi.Host) map[string]*opssightapi.Host {
	existingInternalBlackducks := make(map[string]*opssightapi.Host)
	for _, oldInternalBlackDuck := range oldInternalBlackDucks {
		existingInternalBlackducks[oldInternalBlackDuck.Domain] = oldInternalBlackDuck
	}

	currentInternalBlackducks := make(map[string]*opssightapi.Host)
	for _, newInternalBlackDuck := range newInternalBlackDucks {
		currentInternalBlackducks[newInternalBlackDuck.Domain] = newInternalBlackDuck
	}

	for _, currentInternalBlackduck := range currentInternalBlackducks {
		// check if external host contains the internal host
		if _, ok := existingExternalBlackDucks[currentInternalBlackduck.Domain]; ok {
			// if internal host contains an external host, then check whether it is already part of status,
			// if yes replace it with existing internal host else with new internal host
			if existingInternalBlackduck, ok1 := existingInternalBlackducks[currentInternalBlackduck.Domain]; ok1 {
				existingExternalBlackDucks[currentInternalBlackduck.Domain] = existingInternalBlackduck
			} else {
				existingExternalBlackDucks[currentInternalBlackduck.Domain] = currentInternalBlackduck
			}
		} else {
			// add new internal Black Duck
			existingExternalBlackDucks[currentInternalBlackduck.Domain] = currentInternalBlackduck
		}
	}

	return existingExternalBlackDucks
}
