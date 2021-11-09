/*
This file is part of Cloud Native PostgreSQL.

Copyright (C) 2019-2021 EnterpriseDB Corporation.
*/

package e2e

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/thoas/go-funk"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	apiv1 "github.com/EnterpriseDB/cloud-native-postgresql/api/v1"
	"github.com/EnterpriseDB/cloud-native-postgresql/pkg/utils"
	"github.com/EnterpriseDB/cloud-native-postgresql/tests"
	testsUtils "github.com/EnterpriseDB/cloud-native-postgresql/tests/utils"
)

/*
This test affects the operator itself, so it must be run isolated from the
others.

We test the following:
* A cluster created with the previous (most recent release tag before the actual one) version
  is moved to the current one.
  We test this changing the configuration. That will also perform a switchover.
* A Backup created with the previous version is moved to the current one and
  can be used to bootstrap a cluster.
* A ScheduledBackup created with the previous version is still scheduled after the upgrade.
* A cluster with the previous version is created as a current version one after the upgrade.
* We reply all the previous tests, but we enable the online upgrade in the final CLuster.
*/

var _ = Describe("Upgrade", Label(tests.LabelUpgrade, tests.LabelNoOpenshift), Ordered, func() {
	const (
		operatorNamespace   = "postgresql-operator-system"
		configName          = "postgresql-operator-controller-manager-config"
		operatorUpgradeFile = fixturesDir + "/upgrade/current-manifest.yaml"

		rollingUpgradeNamespace = "rolling-upgrade"
		onlineUpgradeNamespace  = "online-upgrade"

		pgSecrets = fixturesDir + "/upgrade/pgsecrets.yaml" //nolint:gosec

		// This is a cluster of the previous version, created before the operator upgrade
		clusterName1   = "cluster1"
		sampleFile     = fixturesDir + "/upgrade/cluster1.yaml"
		updateConfFile = fixturesDir + "/upgrade/conf-update.yaml"

		// This is a cluster of the previous version, created after the operator upgrade
		clusterName2    = "cluster2"
		sampleFile2     = fixturesDir + "/upgrade/cluster2.yaml"
		updateConfFile2 = fixturesDir + "/upgrade/conf-update2.yaml"

		minioSecret         = fixturesDir + "/upgrade/minio-secret.yaml" //nolint:goƒsec
		minioPVCFile        = fixturesDir + "/upgrade/minio-pvc.yaml"
		minioDeploymentFile = fixturesDir + "/upgrade/minio-deployment.yaml"
		serviceFile         = fixturesDir + "/upgrade/minio-service.yaml"
		clientFile          = fixturesDir + "/upgrade/minio-client.yaml"
		minioClientName     = "mc"
		backupName          = "cluster-backup"
		backupFile          = fixturesDir + "/upgrade/backup1.yaml"
		restoreFile         = fixturesDir + "/upgrade/cluster-restore.yaml"
		scheduledBackupFile = fixturesDir + "/upgrade/scheduled-backup.yaml"
		countBackupsScript  = "sh -c 'mc find minio --name data.tar.gz | wc -l'"
		level               = tests.Lowest
	)

	var upgradeNamespace string

	BeforeEach(func() {
		if testLevelEnv.Depth < int(level) {
			Skip("Test depth is lower than the amount requested for this test")
		}
	})

	JustAfterEach(func() {
		if CurrentSpecReport().Failed() {
			env.DumpClusterEnv(upgradeNamespace, clusterName1,
				"out/"+CurrentSpecReport().LeafNodeText+".log")
		}
	})
	AfterEach(func() {
		err := env.DeleteNamespace(upgradeNamespace)
		Expect(err).ToNot(HaveOccurred())
	})

	// Check that the amount of backups is increasing on minio.
	// This check relies on the fact that nothing is performing backups
	// but a single scheduled backups during the check
	AssertScheduledBackupsAreScheduled := func() {
		By("verifying scheduled backups are still happening", func() {
			out, _, err := tests.Run(fmt.Sprintf(
				"kubectl exec -n %v %v -- %v",
				upgradeNamespace,
				minioClientName,
				countBackupsScript))
			Expect(err).ToNot(HaveOccurred())
			currentBackups, err := strconv.Atoi(strings.Trim(out, "\n"))
			Expect(err).ToNot(HaveOccurred())
			Eventually(func() (int, error) {
				out, _, err := tests.RunUnchecked(fmt.Sprintf(
					"kubectl exec -n %v %v -- %v",
					upgradeNamespace,
					minioClientName,
					countBackupsScript))
				if err != nil {
					return 0, err
				}
				return strconv.Atoi(strings.Trim(out, "\n"))
			}, 120).Should(BeNumerically(">", currentBackups))
		})
	}

	AssertConfUpgrade := func(clusterName string, updateConfFile string) {
		By("checking basic functionality performing a configuration upgrade on the cluster", func() {
			podList, err := env.GetClusterPodList(upgradeNamespace, clusterName)
			Expect(err).ToNot(HaveOccurred())
			// Gather current primary
			namespacedName := types.NamespacedName{
				Namespace: upgradeNamespace,
				Name:      clusterName,
			}
			cluster := &apiv1.Cluster{}
			err = env.Client.Get(env.Ctx, namespacedName, cluster)
			Expect(cluster.Status.CurrentPrimary, err).To(BeEquivalentTo(cluster.Status.TargetPrimary))

			oldPrimary := cluster.Status.CurrentPrimary
			oldPrimaryTimestamp := cluster.Status.CurrentPrimaryTimestamp
			// Update the configuration. It may take some time after the
			// upgrade for the webhook "mcluster.kb.io" to work and accept
			// the `apply` command

			Eventually(func() error {
				_, _, err := tests.RunUnchecked("kubectl apply -n " + upgradeNamespace + " -f " + updateConfFile)
				return err
			}, 60).ShouldNot(HaveOccurred())

			timeout := 300
			commandTimeout := time.Second * 2
			// Check that both parameters have been modified in each pod
			for _, pod := range podList.Items {
				pod := pod // pin the variable
				Eventually(func() (int, error, error) {
					stdout, _, err := env.ExecCommand(env.Ctx, pod, "postgres", &commandTimeout,
						"psql", "-U", "postgres", "-tAc", "show max_replication_slots")
					value, atoiErr := strconv.Atoi(strings.Trim(stdout, "\n"))
					return value, err, atoiErr
				}, timeout).Should(BeEquivalentTo(16),
					"Pod %v should have updated its config", pod.Name)

				Eventually(func() (int, error, error) {
					stdout, _, err := env.ExecCommand(env.Ctx, pod, "postgres", &commandTimeout,
						"psql", "-U", "postgres", "-tAc", "show maintenance_work_mem")
					value, atoiErr := strconv.Atoi(strings.Trim(stdout, "MB\n"))
					return value, err, atoiErr
				}, timeout).Should(BeEquivalentTo(128),
					"Pod %v should have updated its config", pod.Name)
			}
			// Check that a switchover happened
			Eventually(func() (bool, error) {
				c := &apiv1.Cluster{}
				err := env.Client.Get(env.Ctx, namespacedName, c)
				Expect(err).ToNot(HaveOccurred())

				GinkgoWriter.Printf("Current Primary: %s, Current Primary timestamp: %s\n",
					c.Status.CurrentPrimary, c.Status.CurrentPrimaryTimestamp)

				if c.Status.CurrentPrimary != oldPrimary {
					return true, nil
				} else if c.Status.CurrentPrimaryTimestamp != oldPrimaryTimestamp {
					return true, nil
				}

				return false, nil
			}, timeout, "1s").Should(BeTrue())
		})

		By("verifying that all the standbys streams from the primary", func() {
			// To check this we find the primary an create a table on it.
			// The table should be replicated on the standbys.
			primary, err := env.GetClusterPrimary(upgradeNamespace, clusterName)
			Expect(err).ToNot(HaveOccurred())

			commandTimeout := time.Second * 2
			_, _, err = env.ExecCommand(env.Ctx, *primary, "postgres", &commandTimeout,
				"psql", "-U", "postgres", "appdb", "-tAc", "CREATE TABLE postswitch(i int)")
			Expect(err).ToNot(HaveOccurred())

			for i := 1; i < 4; i++ {
				podName := fmt.Sprintf("%v-%v", clusterName, i)
				podNamespacedName := types.NamespacedName{
					Namespace: upgradeNamespace,
					Name:      podName,
				}
				Eventually(func() (string, error) {
					pod := &corev1.Pod{}
					if err := env.Client.Get(env.Ctx, podNamespacedName, pod); err != nil {
						return "", err
					}
					out, _, err := env.ExecCommand(env.Ctx, *pod, "postgres",
						&commandTimeout, "psql", "-U", "postgres", "appdb", "-tAc",
						"SELECT count(*) = 0 FROM postswitch")
					return strings.TrimSpace(out), err
				}, 240).Should(BeEquivalentTo("t"),
					"Pod %v should have followed the new primary", podName)
			}
		})
	}

	assertManagerRollout := func() {
		retryCheckingEvents := wait.Backoff{
			Duration: 10 * time.Second,
			Steps:    5,
		}
		notUpdated := errors.New("notUpdated")
		err := retry.OnError(retryCheckingEvents, func(err error) bool {
			return errors.Is(err, notUpdated)
		}, func() error {
			eventList := corev1.EventList{}
			err := env.Client.List(env.Ctx,
				&eventList,
				ctrlclient.MatchingFields{
					"involvedObject.kind": "Cluster",
					"involvedObject.name": clusterName1,
				},
			)
			if err != nil {
				return err
			}

			var count int
			for _, event := range eventList.Items {
				if event.Reason == "InstanceManagerUpgraded" {
					count++
					GinkgoWriter.Printf("%d: %s\n", count, event.Message)
				}
			}

			if count != 3 {
				return fmt.Errorf("expected 3 online rollouts, but %d happened: %w", count, notUpdated)
			}

			return nil
		})
		Expect(err).NotTo(HaveOccurred())
	}

	applyUpgrade := func(upgradeNamespace string) {
		By(fmt.Sprintf(
			"having a '%s' upgradeNamespace",
			upgradeNamespace), func() {
			// Create a upgradeNamespace for all the resources
			err := env.CreateNamespace(upgradeNamespace)
			Expect(err).ToNot(HaveOccurred())

			// Creating a upgradeNamespace should be quick
			namespacedName := types.NamespacedName{
				Namespace: upgradeNamespace,
				Name:      upgradeNamespace,
			}

			Eventually(func() (string, error) {
				namespaceResource := &corev1.Namespace{}
				err := env.Client.Get(env.Ctx, namespacedName, namespaceResource)
				return namespaceResource.GetName(), err
			}, 20).Should(BeEquivalentTo(upgradeNamespace))
		})

		// Create the secrets used by the clusters and minio
		By("creating the postgres secrets", func() {
			_, _, err := tests.Run(fmt.Sprintf("kubectl apply -n %v -f %v",
				upgradeNamespace, pgSecrets))
			Expect(err).ToNot(HaveOccurred())
		})
		By("creating the cloud storage credentials", func() {
			_, _, err := tests.Run(fmt.Sprintf("kubectl apply -n %v -f %v",
				upgradeNamespace, minioSecret))
			Expect(err).ToNot(HaveOccurred())
		})

		// Create the cluster. Since it will take a while, we'll do more stuff
		// in parallel and check for it to be up later.
		By(fmt.Sprintf("creating a Cluster in the '%v' upgradeNamespace",
			upgradeNamespace), func() {
			Eventually(func() error {
				_, stderr, err := tests.Run(
					"kubectl create -n " + upgradeNamespace + " -f " + sampleFile)
				if err != nil {
					GinkgoWriter.Printf("stderr: %s\n", stderr)
					return err
				}
				return nil
			}, 120).ShouldNot(HaveOccurred())
		})

		// Create the minio deployment and the client in parallel.
		By("creating minio resources", func() {
			// Create a PVC-based deployment for the minio version
			// minio/minio:RELEASE.2020-04-23T00-58-49Z
			_, _, err := tests.Run(fmt.Sprintf("kubectl apply -n %v -f %v",
				upgradeNamespace, minioPVCFile))
			Expect(err).ToNot(HaveOccurred())
			_, _, err = tests.Run(fmt.Sprintf("kubectl apply -n %v -f %v",
				upgradeNamespace, minioDeploymentFile))
			Expect(err).ToNot(HaveOccurred())
			_, _, err = tests.Run(fmt.Sprintf(
				"kubectl apply -n %v -f %v",
				upgradeNamespace, clientFile))
			Expect(err).ToNot(HaveOccurred())
			// Create a minio service
			_, _, err = tests.Run(fmt.Sprintf("kubectl apply -n %v -f %v",
				upgradeNamespace, serviceFile))
			Expect(err).ToNot(HaveOccurred())
		})

		By("having a Cluster with three instances ready", func() {
			AssertClusterIsReady(upgradeNamespace, clusterName1, 600, env)
		})

		By("having minio resources ready", func() {
			// Wait for the minio pod to be ready
			deploymentName := "minio"
			deploymentNamespacedName := types.NamespacedName{
				Namespace: upgradeNamespace,
				Name:      deploymentName,
			}
			Eventually(func() (int32, error) {
				deployment := &appsv1.Deployment{}
				err := env.Client.Get(env.Ctx, deploymentNamespacedName, deployment)
				return deployment.Status.ReadyReplicas, err
			}, 300).Should(BeEquivalentTo(1))

			// Wait for the minio client pod to be ready
			mcNamespacedName := types.NamespacedName{
				Namespace: upgradeNamespace,
				Name:      minioClientName,
			}
			Eventually(func() (bool, error) {
				mc := &corev1.Pod{}
				err := env.Client.Get(env.Ctx, mcNamespacedName, mc)
				return utils.IsPodReady(*mc), err
			}, 180).Should(BeTrue())
		})

		// Now that everything is in place, we add a bit of data we'll use to
		// check if the backup is working
		By("creating data on the database", func() {
			primary := clusterName1 + "-1"
			cmd := "psql -U postgres appdb -tAc 'CREATE TABLE to_restore AS VALUES (1), (2);'"
			_, _, err := tests.Run(fmt.Sprintf(
				"kubectl exec -n %v %v -- %v",
				upgradeNamespace,
				primary,
				cmd))
			Expect(err).ToNot(HaveOccurred())
		})

		// Create a WAL on the primary and check if it arrives on
		// minio within a short time.
		By("archiving WALs on minio", func() {
			primary := clusterName1 + "-1"
			switchWalCmd := "psql -U postgres appdb -tAc 'CHECKPOINT; SELECT pg_walfile_name(pg_switch_wal())'"
			out, _, err := tests.Run(fmt.Sprintf(
				"kubectl exec -n %v %v -- %v",
				upgradeNamespace,
				primary,
				switchWalCmd))
			Expect(err).ToNot(HaveOccurred())
			latestWAL := strings.TrimSpace(out)

			mcName := "mc"
			Eventually(func() (int, error, error) {
				// In the fixture WALs are compressed with gzip
				findCmd := fmt.Sprintf(
					"sh -c 'mc find minio --name %v.gz | wc -l'",
					latestWAL)
				out, _, err := tests.RunUnchecked(fmt.Sprintf(
					"kubectl exec -n %v %v -- %v",
					upgradeNamespace,
					mcName,
					findCmd))

				value, atoiErr := strconv.Atoi(strings.Trim(out, "\n"))
				return value, err, atoiErr
			}, 30).Should(BeEquivalentTo(1))
		})

		By("uploading a backup on minio", func() {
			// We create a Backup
			_, _, err := tests.Run(fmt.Sprintf(
				"kubectl apply -n %v -f %v",
				upgradeNamespace, backupFile))
			Expect(err).ToNot(HaveOccurred())
		})

		By("verifying that a backup has actually completed", func() {
			backupNamespacedName := types.NamespacedName{
				Namespace: upgradeNamespace,
				Name:      backupName,
			}
			Eventually(func() (apiv1.BackupPhase, error) {
				backup := &apiv1.Backup{}
				err := env.Client.Get(env.Ctx, backupNamespacedName, backup)
				return backup.Status.Phase, err
			}, 200).Should(BeEquivalentTo(apiv1.BackupPhaseCompleted))

			// A file called data.tar.gz should be available on minio
			Eventually(func() (int, error, error) {
				out, _, err := tests.RunUnchecked(fmt.Sprintf(
					"kubectl exec -n %v %v -- %v",
					upgradeNamespace,
					minioClientName,
					countBackupsScript))
				value, atoiErr := strconv.Atoi(strings.Trim(out, "\n"))
				return value, err, atoiErr
			}, 30).Should(BeEquivalentTo(1))
		})

		By("creating a ScheduledBackup", func() {
			// We create a ScheduledBackup
			_, _, err := tests.Run(fmt.Sprintf(
				"kubectl apply -n %v -f %v",
				upgradeNamespace, scheduledBackupFile))
			Expect(err).ToNot(HaveOccurred())
		})
		AssertScheduledBackupsAreScheduled()

		var podUIDs []types.UID
		podList, err := env.GetClusterPodList(namespace, clusterName1)
		Expect(err).ToNot(HaveOccurred())
		for _, pod := range podList.Items {
			podUIDs = append(podUIDs, pod.GetUID())
		}

		By("upgrading the operator to current version", func() {
			timeout := 120
			// Upgrade to the new version
			_, _, err := tests.Run(fmt.Sprintf("kubectl apply -f %v", operatorUpgradeFile))
			Expect(err).NotTo(HaveOccurred())
			// With the new deployment, a new pod should be started. When it's
			// ready, the old one is removed. We wait for the number of replicas
			// to decrease to 1.
			Eventually(func() (int32, error) {
				deployment, err := env.GetOperatorDeployment()
				return deployment.Status.Replicas, err
			}, timeout).Should(BeEquivalentTo(1))
			// For a final check, we verify the pod is ready
			Eventually(func() (int32, error) {
				deployment, err := env.GetOperatorDeployment()
				return deployment.Status.ReadyReplicas, err
			}, timeout).Should(BeEquivalentTo(1))
		})

		operatorConfigMapNamespacedName := types.NamespacedName{
			Namespace: operatorNamespace,
			Name:      configName,
		}

		// We need to check here if we were able to upgrade the cluster,
		// be it rolling or online
		// We look for the setting in the operator configMap
		operatorConfigMap := &corev1.ConfigMap{}
		err = env.Client.Get(env.Ctx, operatorConfigMapNamespacedName, operatorConfigMap)
		if err != nil || operatorConfigMap.Data["ENABLE_INSTANCE_MANAGER_INPLACE_UPDATES"] == "false" {
			// Wait for rolling update. We expect all the pods to change UID
			Eventually(func() (int, error) {
				var currentUIDs []types.UID
				currentPodList, err := env.GetClusterPodList(upgradeNamespace, clusterName1)
				if err != nil {
					return 0, err
				}
				for _, pod := range currentPodList.Items {
					currentUIDs = append(currentUIDs, pod.GetUID())
				}
				return len(funk.Join(currentUIDs, podUIDs, funk.InnerJoin).([]types.UID)), nil
			}, 300).Should(BeEquivalentTo(0))
		} else {
			// Pods shouldn't change and there should be an event
			assertManagerRollout()
			Eventually(func() (int, error) {
				var currentUIDs []types.UID
				currentPodList, err := env.GetClusterPodList(upgradeNamespace, clusterName1)
				if err != nil {
					return 0, err
				}
				for _, pod := range currentPodList.Items {
					currentUIDs = append(currentUIDs, pod.GetUID())
				}
				return len(funk.Join(currentUIDs, podUIDs, funk.InnerJoin).([]types.UID)), nil
			}, 300).Should(BeEquivalentTo(3))
		}
		AssertClusterIsReady(upgradeNamespace, clusterName1, 300, env)

		AssertConfUpgrade(clusterName1, updateConfFile)

		By("installing a second Cluster on the upgraded operator", func() {
			_, _, err := tests.Run(
				"kubectl create -n " + upgradeNamespace + " -f " + sampleFile2)
			Expect(err).ToNot(HaveOccurred())

			AssertClusterIsReady(upgradeNamespace, clusterName2, 600, env)
		})

		AssertConfUpgrade(clusterName2, updateConfFile2)

		// We verify that the backup taken before the upgrade is usable to
		// create a v1 cluster
		By("restoring the backup taken from the first Cluster in a new cluster", func() {
			restoredClusterName := "cluster-restore"
			_, _, err := tests.Run(fmt.Sprintf(
				"kubectl apply -n %v -f %v",
				upgradeNamespace, restoreFile))
			Expect(err).ToNot(HaveOccurred())

			AssertClusterIsReady(upgradeNamespace, restoredClusterName, 800, env)

			// Test data should be present on restored primary
			primary := restoredClusterName + "-1"
			cmd := "psql -U postgres appdb -tAc 'SELECT count(*) FROM to_restore'"
			out, _, err := tests.Run(fmt.Sprintf(
				"kubectl exec -n %v %v -- %v",
				upgradeNamespace,
				primary,
				cmd))
			Expect(strings.Trim(out, "\n"), err).To(BeEquivalentTo("2"))

			// Restored primary should be a timeline higher than 1, because
			// we expect a promotion. We can't enforce "2" because the timeline
			// ID will also depend on the history files existing in the cloud
			// storage and we don't know the status of that.
			cmd = "psql -U postgres appdb -tAc 'select substring(pg_walfile_name(pg_current_wal_lsn()), 1, 8)'"
			out, _, err = tests.Run(fmt.Sprintf(
				"kubectl exec -n %v %v -- %v",
				upgradeNamespace,
				primary,
				cmd))
			Expect(err).NotTo(HaveOccurred())
			Expect(strconv.Atoi(strings.Trim(out, "\n"))).To(
				BeNumerically(">", 1))

			// Restored standbys should soon attach themselves to restored primary
			Eventually(func() (string, error) {
				cmd = "psql -U postgres appdb -tAc 'SELECT count(*) FROM pg_stat_replication'"
				out, _, err = tests.Run(fmt.Sprintf(
					"kubectl exec -n %v %v -- %v",
					upgradeNamespace,
					primary,
					cmd))
				return strings.Trim(out, "\n"), err
			}, 180).Should(BeEquivalentTo("2"))
		})
		AssertScheduledBackupsAreScheduled()
	}

	It("works after an upgrade with rolling upgrade ", func() {
		mostRecentTag, err := testsUtils.GetMostRecentReleaseTag("../../releases")
		Expect(err).NotTo(HaveOccurred())

		GinkgoWriter.Printf("installing the recent CNP tag %s\n", mostRecentTag)
		installLatestCNPOperator(mostRecentTag)

		// set upgradeNamespace for log naming
		upgradeNamespace = rollingUpgradeNamespace
		applyUpgrade(upgradeNamespace)
	})

	It("works after an upgrade with online upgrade", func() {
		By("applying environment changes for current upgrade to be performed", func() {
			enableOnlineUpgradeForInstanceManager(operatorNamespace, configName)
		})

		// TODO: Change this By block with the installation of the latest release tag after merging dev/online-update
		// and creating a new release
		By("updating operator image to the testing tag version", func() {
			deployment, err := env.GetOperatorDeployment()
			Expect(err).NotTo(HaveOccurred())

			err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
				var container *corev1.Container

				newImage := "quay.io/enterprisedb/cloud-native-postgresql-testing:online-update"

				Expect(deployment.Spec.Template.Spec.Containers[0].Name).Should(Equal("manager"))
				container = &deployment.Spec.Template.Spec.Containers[0]
				container.Image = newImage
				for i := range container.Env {
					if container.Env[i].Name == "OPERATOR_IMAGE_NAME" {
						container.Env[i].Value = newImage
					}
				}

				return env.Client.Update(env.Ctx, &deployment)
			})
			Expect(err).ShouldNot(HaveOccurred())

			Eventually(func() error {
				stdout, stderr, err := tests.RunUnchecked(
					"kubectl rollout status --timeout=2m -n postgresql-operator-system " +
						"deployment/postgresql-operator-controller-manager")
				GinkgoWriter.Printf("stdout: %s\n", stdout)
				GinkgoWriter.Printf("stderr: %s\n", stderr)
				return err
			}, 150).ShouldNot(HaveOccurred())
		})

		// set upgradeNamespace for log naming
		upgradeNamespace = onlineUpgradeNamespace
		applyUpgrade(upgradeNamespace)

		assertManagerRollout()
	})
})

func enableOnlineUpgradeForInstanceManager(pgOperatorNamespace, configName string) {
	By("creating operator namespace", func() {
		// Create a upgradeNamespace for all the resources
		namespacedName := types.NamespacedName{
			Name: pgOperatorNamespace,
		}
		namespaceResource := &corev1.Namespace{}
		err := env.Client.Get(env.Ctx, namespacedName, namespaceResource)
		if apierrors.IsNotFound(err) {
			err = env.CreateNamespace(pgOperatorNamespace)
			Expect(err).ToNot(HaveOccurred())
		} else if err != nil {
			Expect(err).ToNot(HaveOccurred())
		}
	})

	By("ensuring 'ENABLE_INSTANCE_MANAGER_INPLACE_UPDATES' is set to true", func() {
		configMap := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: pgOperatorNamespace,
				Name:      configName,
			},
			Data: map[string]string{"ENABLE_INSTANCE_MANAGER_INPLACE_UPDATES": "true"},
		}
		err := env.Client.Create(env.Ctx, configMap)
		Expect(err).NotTo(HaveOccurred())
	})
}

// install an operator version with the most recent release tag
func installLatestCNPOperator(releaseTag string) {
	mostRecentReleasePath := "../../releases/postgresql-operator-" + releaseTag + ".yaml"

	Eventually(func() error {
		GinkgoWriter.Printf("installing: %s\n", mostRecentReleasePath)

		_, stderr, err := tests.RunUnchecked("kubectl apply -f " + mostRecentReleasePath)
		if err != nil {
			GinkgoWriter.Printf("stderr: %s\n", stderr)
		}

		return err
	}, 60).ShouldNot(HaveOccurred())

	Eventually(func() error {
		_, _, err := tests.RunUnchecked(
			"kubectl wait --for condition=established --timeout=60s " +
				"crd/clusters.postgresql.k8s.enterprisedb.io")
		return err
	}, 150).ShouldNot(HaveOccurred())

	Eventually(func() error {
		mapping, err := env.Client.RESTMapper().RESTMapping(
			schema.GroupKind{Group: apiv1.GroupVersion.Group, Kind: apiv1.ClusterKind},
			apiv1.GroupVersion.Version)
		if err != nil {
			return err
		}

		GinkgoWriter.Printf("found mapping REST endpoint: %s\n", mapping.GroupVersionKind.String())

		return nil
	}, 150).ShouldNot(HaveOccurred())

	Eventually(func() error {
		_, _, err := tests.RunUnchecked(
			"kubectl wait --for=condition=Available --timeout=2m -n postgresql-operator-system " +
				"deployments postgresql-operator-controller-manager")
		return err
	}, 150).ShouldNot(HaveOccurred())
}