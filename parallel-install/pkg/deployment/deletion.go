//Package deployment provides a top-level API to control Kyma deployment and uninstallation.
package deployment

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/avast/retry-go"
	"github.com/kubernetes-sigs/service-catalog/pkg/client/clientset_generated/clientset"
	"github.com/kyma-incubator/hydroform/parallel-install/pkg/components"
	"github.com/kyma-incubator/hydroform/parallel-install/pkg/config"
	"github.com/kyma-incubator/hydroform/parallel-install/pkg/engine"
	"github.com/kyma-incubator/hydroform/parallel-install/pkg/helm"
	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

//Deletion removes Kyma from a cluster
type Deletion struct {
	*core
	mp           *helm.KymaMetadataProvider
	scclient     *clientset.Clientset
	dClient      dynamic.Interface
	retryOptions []retry.Option
}

//NewDeletion creates a new Deployment instance for deleting Kyma on a cluster.
func NewDeletion(cfg *config.Config, ob *OverridesBuilder, processUpdates func(ProcessUpdate), retryOptions []retry.Option) (*Deletion, error) {
	if err := cfg.ValidateDeletion(); err != nil {
		return nil, err
	}

	restConfig, err := config.RestConfig(cfg.KubeconfigSource)
	if err != nil {
		return nil, err
	}

	kubeClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}

	scclient, err := clientset.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}

	dClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}
	registerOverridesInterceptors(ob, kubeClient, cfg.Log)

	core := newCore(cfg, ob, kubeClient, processUpdates)

	mp, err := helm.NewKymaMetadataProvider(cfg.KubeconfigSource)
	if err != nil {
		return nil, err
	}

	return &Deletion{core, mp, scclient, dClient, retryOptions}, nil
}

//StartKymaUninstallation removes Kyma from a cluster
func (i *Deletion) StartKymaUninstallation() error {
	_, prerequisitesEng, componentsEng, err := i.getConfig()
	if err != nil {
		return err
	}

	return i.startKymaUninstallation(prerequisitesEng, componentsEng)
}

func (i *Deletion) startKymaUninstallation(prerequisitesEng *engine.Engine, componentsEng *engine.Engine) error {
	i.cfg.Log.Info("Kyma uninstallation started")

	cancelCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cancelTimeout := i.cfg.CancelTimeout
	quitTimeout := i.cfg.QuitTimeout

	namespaces, err := i.mp.Namespaces()
	if err != nil {
		return err
	}
	//TODO: Delete this when kyma-installer is not used any more.
	namespaces = append(namespaces, "kyma-installer")

	startTime := time.Now()
	err = i.uninstallComponents(cancelCtx, cancel, UninstallComponents, componentsEng, cancelTimeout, quitTimeout)
	if err != nil {
		return err
	}
	endTime := time.Now()

	i.cfg.Log.Info("Kyma prerequisites uninstallation")

	cancelTimeout = calculateDuration(startTime, endTime, i.cfg.CancelTimeout)
	quitTimeout = calculateDuration(startTime, endTime, i.cfg.QuitTimeout)

	err = i.uninstallComponents(cancelCtx, cancel, UninstallPreRequisites, prerequisitesEng, cancelTimeout, quitTimeout)
	if err != nil {
		return err
	}

	return i.deleteKymaNamespaces(namespaces)
}

func (i *Deletion) uninstallComponents(ctx context.Context, cancelFunc context.CancelFunc, phase InstallationPhase, eng *engine.Engine, cancelTimeout time.Duration, quitTimeout time.Duration) error {
	cancelTimeoutChan := time.After(cancelTimeout)
	quitTimeoutChan := time.After(quitTimeout)
	var statusMap = map[string]string{}
	var errCount int = 0
	var timeoutOccured bool = false

	statusChan, err := eng.Uninstall(ctx)
	if err != nil {
		return err
	}

	i.processUpdate(phase, ProcessStart, nil)

	//Await completion
UninstallLoop:
	for {
		select {
		case cmp, ok := <-statusChan:
			if ok {
				i.processUpdateComponent(phase, cmp)
				if cmp.Status == components.StatusError {
					errCount++
				}
				statusMap[cmp.Name] = cmp.Status
			} else {
				if errCount > 0 {
					err := fmt.Errorf("Kyma uninstallation failed due to errors in %d component(s)", errCount)
					i.processUpdate(phase, ProcessExecutionFailure, err)
					i.logStatuses(statusMap)
					return err
				}
				if timeoutOccured {
					err := fmt.Errorf("Kyma uninstallation failed due to the timeout")
					i.processUpdate(phase, ProcessTimeoutFailure, err)
					i.logStatuses(statusMap)
					return err
				}
				break UninstallLoop
			}
		case <-cancelTimeoutChan:
			timeoutOccured = true
			i.cfg.Log.Errorf("Timeout occurred after %v minutes. Cancelling uninstallation", cancelTimeout.Minutes())
			cancelFunc()
		case <-quitTimeoutChan:
			err := fmt.Errorf("Force quit: Kyma uninstallation failed due to the timeout")
			i.processUpdate(phase, ProcessForceQuitFailure, err)
			i.cfg.Log.Error("Uninstallation doesn't stop after it's canceled. Enforcing quit")
			return err
		}
	}
	i.processUpdate(phase, ProcessFinished, nil)
	return nil
}

func (i *Deletion) deleteKymaNamespaces(namespaces []string) error {
	var wg sync.WaitGroup
	wg.Add(len(namespaces))

	finishedCh := make(chan bool)
	errorCh := make(chan error)

	// start deletion in goroutines
	for _, namespace := range namespaces {
		err := retry.Do(func() error {
			// Check if there are any running Pods left on the namespace
			pods, err := i.kubeClient.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{})
			if err != nil {
				errorCh <- err
			}

			if len(pods.Items) > 0 {
				for _, pod := range pods.Items {
					if pod.Status.Phase == v1.PodRunning {
						return errors.New(fmt.Sprintf("Namespace %s could not be deleted because of the running Pod: %s. Trying again..", namespace, pod.Name))
					}
				}
			}
			return nil
		}, i.retryOptions...)

		if err != nil {
			i.cfg.Log.Infof("Namespace %s could not be deleted because of running Pod(s)", namespace)
			wg.Done()
			continue
		}

		go func(ns string) {
			defer wg.Done()
			if ns == "kyma-system" {
				//HACK: Delete finalizers of leftover Cluster Service Brokers
				csbList, err := i.scclient.ServicecatalogV1beta1().ClusterServiceBrokers().List(context.Background(), metav1.ListOptions{})
				if err != nil {
					errorCh <- err
				}
				for _, csb := range csbList.Items {
					csb.Finalizers = []string{}
					_, err := i.scclient.ServicecatalogV1beta1().ClusterServiceBrokers().Update(context.Background(), &csb, metav1.UpdateOptions{})
					if err != nil {
						errorCh <- err
					}
					i.cfg.Log.Infof("Deleted finalizer from CSB: %s", csb.Name)
				}

				//HACK: Delete finalizers of leftover Service Brokers
				sbList, err := i.scclient.ServicecatalogV1beta1().ServiceBrokers(ns).List(context.Background(), metav1.ListOptions{})
				if err != nil {
					errorCh <- err
				}
				for _, sb := range sbList.Items {
					sb.Finalizers = []string{}
					_, err := i.scclient.ServicecatalogV1beta1().ServiceBrokers(ns).Update(context.Background(), &sb, metav1.UpdateOptions{})
					if err != nil {
						errorCh <- err
					}
					i.cfg.Log.Infof("Deleted finalizer from SB: %s", sb.Name)
				}

				//HACK: Delete finalizers of leftover Secret
				secret, err := i.kubeClient.CoreV1().Secrets(ns).Get(context.Background(), "serverless-registry-config-default", metav1.GetOptions{})
				if err != nil && !apierr.IsNotFound(err) {
					errorCh <- err
				}
				if secret != nil {
					secret.Finalizers = []string{}
					if _, err := i.kubeClient.CoreV1().Secrets(ns).Update(context.Background(), secret, metav1.UpdateOptions{}); err != nil {
						errorCh <- err
					}
					i.cfg.Log.Infof("Deleted finalizer from Secret: %s", secret.Name)
				}

				//HACK: Delete finalizers of leftover ORY Rules
				ruleResource := schema.GroupVersionResource{
					Group:    "oathkeeper.ory.sh",
					Version:  "v1alpha1",
					Resource: "rules",
				}

				rules, err := i.dClient.Resource(ruleResource).Namespace(ns).List(context.Background(), metav1.ListOptions{})
				if err != nil {
					errorCh <- err
				}
				for _, rule := range rules.Items {
					rule.SetFinalizers(nil)
					_, err := i.dClient.Resource(ruleResource).Namespace(ns).Update(context.Background(), &rule, metav1.UpdateOptions{})
					if err != nil {
						errorCh <- err
					}
					i.cfg.Log.Infof("Deleted finalizer from Rule: %s", rule.GetName())
				}
			}
			//remove namespace
			if err := i.kubeClient.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{}); err != nil && !apierr.IsNotFound(err) {
				errorCh <- err
			}
			i.cfg.Log.Infof("Namespace '%s' is removed", ns)
		}(namespace)
	}

	// wait until parallel deletion is finished
	go func() {
		wg.Wait()
		close(errorCh)
		close(finishedCh)
	}()

	// process deletion results
	var errWrapped error
	for {
		select {
		case <-finishedCh:
			return errWrapped
		case err := <-errorCh:
			if err != nil {
				if errWrapped == nil {
					errWrapped = err
				} else {
					errWrapped = errors.Wrap(err, errWrapped.Error())
				}
			}
		}
	}
}
