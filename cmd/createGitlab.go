package cmd

import (
	"crypto/tls"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"syscall"
	"time"

	"github.com/kubefirst/kubefirst/configs"
	"github.com/kubefirst/kubefirst/internal/argocd"
	"github.com/kubefirst/kubefirst/internal/flagset"
	"github.com/kubefirst/kubefirst/internal/gitlab"
	"github.com/kubefirst/kubefirst/internal/helm"
	"github.com/kubefirst/kubefirst/internal/k8s"
	"github.com/kubefirst/kubefirst/internal/progressPrinter"
	"github.com/kubefirst/kubefirst/internal/softserve"
	"github.com/kubefirst/kubefirst/internal/terraform"
	"github.com/kubefirst/kubefirst/internal/vault"
	"github.com/kubefirst/kubefirst/pkg"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// createGitlabCmd represents the createGitlab command
var createGitlabCmd = &cobra.Command{
	Use:   "create-gitlab",
	Short: "create a kubefirst management cluster",
	Long:  `TBD`,
	RunE: func(cmd *cobra.Command, args []string) error {

		config := configs.ReadConfig()

		fmt.Println("createGitlab called")
		skipVault, err := cmd.Flags().GetBool("skip-vault")
		if err != nil {
			log.Panic(err)
		}
		skipGitlab, err := cmd.Flags().GetBool("skip-gitlab")
		if err != nil {
			log.Panic(err)
		}

		globalFlags, err := flagset.ProcessGlobalFlags(cmd)
		if err != nil {
			return err
		}
		//infoCmd need to be before the bars or it is printed in between bars:
		//Let's try to not move it on refactors
		infoCmd.Run(cmd, args)
		progressPrinter.GetInstance()
		progressPrinter.SetupProgress(4, globalFlags.SilentMode)

		var kPortForwardArgocd *exec.Cmd
		progressPrinter.AddTracker("step-0", "Process Parameters", 1)

		progressPrinter.IncrementTracker("step-0", 1)

		progressPrinter.AddTracker("step-softserve", "Prepare Temporary Repo ", 4)
		progressPrinter.IncrementTracker("step-softserve", 1)
		if !globalFlags.UseTelemetry {
			informUser("Telemetry Disabled", globalFlags.SilentMode)
		}
		directory := fmt.Sprintf("%s/gitops/terraform/base", config.K1FolderPath)
		informUser("Creating K8S Cluster", globalFlags.SilentMode)
		terraform.ApplyBaseTerraform(globalFlags.DryRun, directory)
		progressPrinter.IncrementTracker("step-softserve", 1)

		restoreSSLCmd.RunE(cmd, args)

		clientset, err := k8s.GetClientSet(globalFlags.DryRun)
		if err != nil {
			panic(err.Error())
		}

		//! soft-serve was just applied

		softserve.CreateSoftServe(globalFlags.DryRun, config.KubeConfigPath)
		informUser("Created Softserve", globalFlags.SilentMode)
		progressPrinter.IncrementTracker("step-softserve", 1)
		informUser("Waiting Softserve", globalFlags.SilentMode)
		k8s.WaitForNamespaceandPods(globalFlags.DryRun, config, "soft-serve", "app=soft-serve")
		progressPrinter.IncrementTracker("step-softserve", 1)
		// todo this should be replaced with something more intelligent
		log.Println("Waiting for soft-serve installation to complete...")

		totalAttempts := 10
		var kPortForwardSoftServe *exec.Cmd
		for i := 0; i < totalAttempts; i++ {

			kPortForwardSoftServe, err = k8s.PortForward(globalFlags.DryRun, "soft-serve", "svc/soft-serve", "8022:22")
			defer func() {
				_ = kPortForwardSoftServe.Process.Signal(syscall.SIGTERM)
			}()
			if err != nil {
				log.Println("Error creating port-forward")
				return err
			}
			time.Sleep(20 * time.Second)

			err = softserve.ConfigureSoftServeAndPush(globalFlags.DryRun)
			if viper.GetBool("create.softserve.configure") || err == nil {
				log.Println("Soft-serve configured")
				break
			} else {
				log.Println("Soft-serve not configured - waiting before trying again")
				log.Println("Soft-serve not configured - Re-creating Port-forward deails at: https://github.com/kubefirst/kubefirst/issues/429")
				time.Sleep(20 * time.Second)
				_ = kPortForwardSoftServe.Process.Signal(syscall.SIGTERM)
			}
		}
		informUser("Softserve Update", globalFlags.SilentMode)
		progressPrinter.IncrementTracker("step-softserve", 1)

		progressPrinter.AddTracker("step-argo", "Deploy CI/CD ", 5)
		informUser("Deploy ArgoCD", globalFlags.SilentMode)
		progressPrinter.IncrementTracker("step-argo", 1)
		err = helm.InstallArgocd(globalFlags.DryRun)
		if err != nil {
			log.Println("Error installing argocd")
			return err
		}

		//! argocd was just helm installed
		waitArgoCDToBeReady(globalFlags.DryRun)

		informUser("ArgoCD Ready", globalFlags.SilentMode)
		progressPrinter.IncrementTracker("step-argo", 1)

		if !globalFlags.DryRun {
			kPortForwardArgocd, err = k8s.PortForward(globalFlags.DryRun, "argocd", "svc/argocd-server", "8080:80")
			defer kPortForwardArgocd.Process.Signal(syscall.SIGTERM)
			if err != nil {
				log.Println("Error creating port-forward")
				return err
			}

		}

		// log.Println("sleeping for 45 seconds, hurry up jared")
		// time.Sleep(45 * time.Second)

		informUser(fmt.Sprintf("ArgoCD available at %s", viper.GetString("argocd.local.service")), globalFlags.SilentMode)
		progressPrinter.IncrementTracker("step-argo", 1)

		informUser("Setting argocd credentials", globalFlags.SilentMode)
		setArgocdCreds(globalFlags.DryRun)
		progressPrinter.IncrementTracker("step-argo", 1)

		informUser("Getting an argocd auth token", globalFlags.SilentMode)

		progressPrinter.IncrementTracker("step-argo", 1)
		if !globalFlags.DryRun {
			_, _, err = pkg.ExecShellReturnStrings(config.KubectlClientPath, "--kubeconfig", config.KubeConfigPath, "-n", "argocd", "apply", "-f", fmt.Sprintf("%s/gitops/components/helpers/registry-softserve.yaml", config.K1FolderPath))
			if err != nil {
				log.Panicf("failed to call execute kubectl apply of argocd patch to adopt gitlab: %s", err)
			}
			time.Sleep(45 * time.Second)
		}
		progressPrinter.IncrementTracker("step-argo", 1)

		//!
		//* we need to stop here and wait for the vault namespace to exist and the vault pod to be ready
		//!
		progressPrinter.AddTracker("step-gitlab", "Setup Gitlab", 6)
		informUser("Waiting vault to be ready", globalFlags.SilentMode)
		waitVaultToBeRunning(globalFlags.DryRun)
		progressPrinter.IncrementTracker("step-gitlab", 1)
		if !globalFlags.DryRun {
			kPortForwardVault, err := k8s.PortForward(globalFlags.DryRun, "vault", "svc/vault", "8200:8200")
			defer kPortForwardVault.Process.Signal(syscall.SIGTERM)
			if err != nil {
				log.Println("Error creating port-forward")
				return err
			}

		}
		/*
			// Testing gitlab HTTPS creation, vaults needs gitlab
			for i := 1; i < 15; i++ {
				hostReady := gitlab.AwaitHostNTimes("gitlab", globalFlags.DryRun, 20)
				if hostReady {
					informUser("gitlab DNS is ready", globalFlags.SilentMode)
					break
				} else {
					informUser("gitlab DNS is not ready", globalFlags.SilentMode)
				}
			}
		*/
		loopUntilPodIsReady(globalFlags.DryRun)
		initializeVaultAndAutoUnseal(globalFlags.DryRun)
		informUser(fmt.Sprintf("Vault available at %s", viper.GetString("vault.local.service")), globalFlags.SilentMode)
		progressPrinter.IncrementTracker("step-gitlab", 1)

		informUser("Waiting gitlab to be ready", globalFlags.SilentMode)
		waitGitlabToBeReady(globalFlags.DryRun)
		log.Println("waiting for gitlab")
		k8s.WaitForGitlab(globalFlags.DryRun, config)
		log.Println("gitlab is ready!")
		progressPrinter.IncrementTracker("step-gitlab", 1)

		if !globalFlags.DryRun {
			kPortForwardGitlab, err := k8s.PortForward(globalFlags.DryRun, "gitlab", "svc/gitlab-webservice-default", "8888:8080")
			defer kPortForwardGitlab.Process.Signal(syscall.SIGTERM)
			if err != nil {
				log.Println("Error creating port-forward")
				return err
			}
		}
		informUser(fmt.Sprintf("Gitlab available at %s", viper.GetString("gitlab.local.service")), globalFlags.SilentMode)
		progressPrinter.IncrementTracker("step-gitlab", 1)

		if !skipGitlab {
			// TODO: Confirm if we need to waitgit lab to be ready
			// OR something, too fast the secret will not be there.
			informUser("Gitlab setup tokens", globalFlags.SilentMode)
			gitlab.ProduceGitlabTokens(globalFlags.DryRun)
			progressPrinter.IncrementTracker("step-gitlab", 1)
			informUser("Gitlab terraform", globalFlags.SilentMode)
			gitlab.ApplyGitlabTerraform(globalFlags.DryRun, directory)
			gitlab.GitlabKeyUpload(globalFlags.DryRun)
			informUser("Gitlab ready", globalFlags.SilentMode)
			progressPrinter.IncrementTracker("step-gitlab", 1)
		}
		if !skipVault {

			progressPrinter.AddTracker("step-vault", "Configure Vault", 2)
			informUser("waiting for vault unseal", globalFlags.SilentMode)

			log.Println("configuring vault")
			vault.ConfigureVault(globalFlags.DryRun, true)
			informUser("Vault configured", globalFlags.SilentMode)
			progressPrinter.IncrementTracker("step-vault", 1)

			log.Println("creating vault configured secret")
			k8s.CreateVaultConfiguredSecret(globalFlags.DryRun, config)
			informUser("Vault  secret created", globalFlags.SilentMode)
			progressPrinter.IncrementTracker("step-vault", 1)
		}
		progressPrinter.AddTracker("step-post-gitlab", "Finalize Gitlab updates", 3)
		if !viper.GetBool("gitlab.oidc-created") {
			vault.AddGitlabOidcApplications(globalFlags.DryRun)
			informUser("Added Gitlab OIDC", globalFlags.SilentMode)

			informUser("Waiting for Gitlab dns to propagate before continuing", globalFlags.SilentMode)
			gitlab.AwaitHost("gitlab", globalFlags.DryRun)
			informUser("Pushing gitops repo to origin gitlab", globalFlags.SilentMode)
			// refactor: sounds like a new functions, should PushGitOpsToGitLab be renamed/update signature?
			viper.Set("gitlab.oidc-created", true)
			viper.WriteConfig()
		}
		progressPrinter.IncrementTracker("step-post-gitlab", 1)
		if !viper.GetBool("gitlab.gitops-pushed") {
			gitlab.PushGitRepo(globalFlags.DryRun, config, "gitlab", "gitops") // todo: need to handle if this was already pushed, errors on failure)
			// todo: keep one of the two git push functions, they're similar, but not exactly the same
			//gitlab.PushGitOpsToGitLab(globalFlags.DryRun)
			viper.Set("gitlab.gitops-pushed", true)
			viper.WriteConfig()
		}
		progressPrinter.IncrementTracker("step-post-gitlab", 1)
		if !globalFlags.DryRun && !viper.GetBool("argocd.oidc-patched") {
			argocd.ArgocdSecretClient = clientset.CoreV1().Secrets("argocd")
			k8s.PatchSecret(argocd.ArgocdSecretClient, "argocd-secret", "oidc.gitlab.clientSecret", viper.GetString("gitlab.oidc.argocd.secret"))

			argocdPodClient := clientset.CoreV1().Pods("argocd")
			k8s.DeletePodByLabel(argocdPodClient, "app.kubernetes.io/name=argocd-server")
			viper.Set("argocd.oidc-patched", true)
			viper.WriteConfig()
		}

		if !viper.GetBool("gitlab.registered") {
			// informUser("Getting ArgoCD auth token
			// token := argocd.GetArgocdAuthToken(globalFlags.DryRun)

			// informUser("Detaching the registry application from softserve
			// argocd.DeleteArgocdApplicationNoCascade(globalFlags.DryRun, "registry", token)

			informUser("Adding the registry application registered against gitlab", globalFlags.SilentMode)
			gitlab.ChangeRegistryToGitLab(globalFlags.DryRun)
			// todo triage / force apply the contents adjusting
			// todo kind: Application .repoURL:

			// informUser("Waiting for argocd host to resolve
			// gitlab.AwaitHost("argocd", globalFlags.DryRun)
			if !globalFlags.DryRun {
				argocdPodClient := clientset.CoreV1().Pods("argocd")
				kPortForwardArgocd.Process.Signal(syscall.SIGTERM)
				informUser("deleting argocd-server pod", globalFlags.SilentMode)
				k8s.DeletePodByLabel(argocdPodClient, "app.kubernetes.io/name=argocd-server")
			}
			informUser("waiting for argocd to be ready", globalFlags.SilentMode)
			waitArgoCDToBeReady(globalFlags.DryRun)

			informUser("Port forwarding to new argocd-server pod", globalFlags.SilentMode)
			if !globalFlags.DryRun {
				time.Sleep(time.Second * 20)
				kPortForwardArgocd, err = k8s.PortForward(globalFlags.DryRun, "argocd", "svc/argocd-server", "8080:80")
				defer kPortForwardArgocd.Process.Signal(syscall.SIGTERM)
				if err != nil {
					log.Println("Error creating port-forward")
					return err
				}
				log.Println("sleeping for 40 seconds")
				time.Sleep(40 * time.Second)
			}

			informUser("Syncing the registry application", globalFlags.SilentMode)
			token := argocd.GetArgocdAuthToken(globalFlags.DryRun)

			if globalFlags.DryRun {
				log.Printf("[#99] Dry-run mode, Sync ArgoCD skipped")
			} else {
				// todo: create ArgoCD struct, and host dependencies (like http client)
				customTransport := http.DefaultTransport.(*http.Transport).Clone()
				customTransport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
				httpClient := http.Client{Transport: customTransport}

				// retry to sync ArgoCD application until reaches the maximum attempts
				argoCDIsReady, err := argocd.SyncRetry(&httpClient, 120, 5, "registry", token)
				if err != nil {
					log.Printf("something went wrong during ArgoCD sync step, error is: %v", err)
				}

				if !argoCDIsReady {
					log.Println("unable to sync ArgoCD application, continuing...")
				}
			}

			viper.Set("gitlab.registered", true)
			viper.WriteConfig()
		}
		progressPrinter.IncrementTracker("step-post-gitlab", 1)
		//!--
		// Wait argocd cert to work, or force restart
		if !globalFlags.DryRun {
			argocdPodClient := clientset.CoreV1().Pods("argocd")
			for i := 1; i < 15; i++ {
				argoCDHostReady := gitlab.AwaitHostNTimes("argocd", globalFlags.DryRun, 20)
				if argoCDHostReady {
					informUser("ArgoCD DNS is ready", globalFlags.SilentMode)
					break
				} else {
					k8s.DeletePodByLabel(argocdPodClient, "app.kubernetes.io/name=argocd-server")
				}
			}
		}

		//!--

		if !skipVault {
			progressPrinter.AddTracker("step-vault-be", "Configure Vault Backend", 1)
			log.Println("configuring vault backend")
			vault.ConfigureVault(globalFlags.DryRun, false)
			informUser("Vault backend configured", globalFlags.SilentMode)
			progressPrinter.IncrementTracker("step-vault-be", 1)
		}
		return nil
	},
}

func init() {
	clusterCmd.AddCommand(createGitlabCmd)
	currentCommand := createGitlabCmd
	// todo: make this an optional switch and check for it or viper
	currentCommand.Flags().Bool("destroy", false, "destroy resources")
	currentCommand.Flags().Bool("skip-gitlab", false, "Skip GitLab lab install and vault setup")
	currentCommand.Flags().Bool("skip-vault", false, "Skip post-gitClient lab install and vault setup")
	flagset.DefineGlobalFlags(currentCommand)
}