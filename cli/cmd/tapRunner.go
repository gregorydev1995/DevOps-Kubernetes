package cmd

import (
	"context"
	"fmt"
	"github.com/up9inc/mizu/shared"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"time"

	core "k8s.io/api/core/v1"

	"github.com/up9inc/mizu/cli/debounce"
	"github.com/up9inc/mizu/cli/kubernetes"
	"github.com/up9inc/mizu/cli/mizu"
)

var mizuServiceAccountExists bool
var aggregatorService *core.Service

const (
	updateTappersDelay = 5 * time.Second
)

var currentlyTappedPods []core.Pod

func RunMizuTap(podRegexQuery *regexp.Regexp, tappingOptions *MizuTapOptions) {
	mizuApiFilteringOptions, err := getMizuApiFilteringOptions(tappingOptions)
	if err != nil {
		return
	}

	kubernetesProvider := kubernetes.NewProvider(tappingOptions.KubeConfigPath)

	defer cleanUpMizuResources(kubernetesProvider)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // cancel will be called when this function exits

	if matchingPods, err := kubernetesProvider.GetAllPodsMatchingRegex(ctx, podRegexQuery); err != nil {
		return
	} else {
		currentlyTappedPods = matchingPods
	}

	nodeToTappedPodIPMap, err := getNodeHostToTappedPodIpsMap(currentlyTappedPods)
	if err != nil {
		return
	}

	if err := createMizuResources(ctx, kubernetesProvider, nodeToTappedPodIPMap, tappingOptions, mizuApiFilteringOptions); err != nil {
		return
	}

	go portForwardApiPod(ctx, kubernetesProvider, cancel, tappingOptions) // TODO convert this to job for built in pod ttl or have the running app handle this
	go watchPodsForTapping(ctx, kubernetesProvider, cancel, podRegexQuery, tappingOptions)
	go syncApiStatus(ctx, cancel, tappingOptions)

	//block until exit signal or error
	waitForFinish(ctx, cancel)

	// TODO handle incoming traffic from tapper using a channel
}

func createMizuResources(ctx context.Context, kubernetesProvider *kubernetes.Provider, nodeToTappedPodIPMap map[string][]string, tappingOptions *MizuTapOptions, mizuApiFilteringOptions *shared.TrafficFilteringOptions) error {
	if err := createMizuAggregator(ctx, kubernetesProvider, tappingOptions, mizuApiFilteringOptions); err != nil {
		return err
	}

	if err := createMizuTappers(ctx, kubernetesProvider, nodeToTappedPodIPMap, tappingOptions); err != nil {
		return err
	}

	return nil
}

func createMizuAggregator(ctx context.Context, kubernetesProvider *kubernetes.Provider, tappingOptions *MizuTapOptions, mizuApiFilteringOptions *shared.TrafficFilteringOptions) error {
	var err error

	mizuServiceAccountExists = createRBACIfNecessary(ctx, kubernetesProvider)
	_, err = kubernetesProvider.CreateMizuAggregatorPod(ctx, mizu.ResourcesNamespace, mizu.AggregatorPodName, tappingOptions.MizuImage, mizuServiceAccountExists, mizuApiFilteringOptions)
	if err != nil {
		fmt.Printf("Error creating mizu collector pod: %v\n", err)
		return err
	}

	aggregatorService, err = kubernetesProvider.CreateService(ctx, mizu.ResourcesNamespace, mizu.AggregatorPodName, mizu.AggregatorPodName)
	if err != nil {
		fmt.Printf("Error creating mizu collector service: %v\n", err)
		return err
	}

	return nil
}

func getMizuApiFilteringOptions(tappingOptions *MizuTapOptions) (*shared.TrafficFilteringOptions, error) {
	if tappingOptions.PlainTextFilterRegexes == nil || len(tappingOptions.PlainTextFilterRegexes) == 0 {
		return nil, nil
	}

	compiledRegexSlice := make([]*shared.SerializableRegexp, 0)
	for _, regexStr := range tappingOptions.PlainTextFilterRegexes {
		compiledRegex, err := shared.CompileRegexToSerializableRegexp(regexStr)
		if err != nil {
			fmt.Printf("Regex %s is invalid: %v", regexStr, err)
			return nil, err
		}
		compiledRegexSlice = append(compiledRegexSlice, compiledRegex)
	}

	return &shared.TrafficFilteringOptions{PlainTextMaskingRegexes: compiledRegexSlice}, nil
}

func createMizuTappers(ctx context.Context, kubernetesProvider *kubernetes.Provider, nodeToTappedPodIPMap map[string][]string, tappingOptions *MizuTapOptions) error {
	if err := kubernetesProvider.ApplyMizuTapperDaemonSet(
		ctx,
		mizu.ResourcesNamespace,
		mizu.TapperDaemonSetName,
		tappingOptions.MizuImage,
		mizu.TapperPodName,
		fmt.Sprintf("%s.%s.svc.cluster.local", aggregatorService.Name, aggregatorService.Namespace),
		nodeToTappedPodIPMap,
		mizuServiceAccountExists,
	); err != nil {
		fmt.Printf("Error creating mizu tapper daemonset: %v\n", err)
		return err
	}

	return nil
}

func cleanUpMizuResources(kubernetesProvider *kubernetes.Provider) {
	fmt.Printf("\nRemoving mizu resources\n")

	removalCtx, _ := context.WithTimeout(context.Background(), 5*time.Second)
	if err := kubernetesProvider.RemovePod(removalCtx, mizu.ResourcesNamespace, mizu.AggregatorPodName); err != nil {
		fmt.Printf("Error removing Pod %s in namespace %s: %s (%v,%+v)\n", mizu.AggregatorPodName, mizu.ResourcesNamespace, err, err, err)
	}
	if err := kubernetesProvider.RemoveService(removalCtx, mizu.ResourcesNamespace, mizu.AggregatorPodName); err != nil {
		fmt.Printf("Error removing Service %s in namespace %s: %s (%v,%+v)\n", mizu.AggregatorPodName, mizu.ResourcesNamespace, err, err, err)
	}
	if err := kubernetesProvider.RemoveDaemonSet(removalCtx, mizu.ResourcesNamespace, mizu.TapperDaemonSetName); err != nil {
		fmt.Printf("Error removing DaemonSet %s in namespace %s: %s (%v,%+v)\n", mizu.TapperDaemonSetName, mizu.ResourcesNamespace, err, err, err)
	}
}

func watchPodsForTapping(ctx context.Context, kubernetesProvider *kubernetes.Provider, cancel context.CancelFunc, podRegex *regexp.Regexp, tappingOptions *MizuTapOptions) {
	added, modified, removed, errorChan := kubernetes.FilteredWatch(ctx, kubernetesProvider.GetPodWatcher(ctx, getNamespace(tappingOptions, kubernetesProvider)), podRegex)

	restartTappers := func() {
		if matchingPods, err := kubernetesProvider.GetAllPodsMatchingRegex(ctx, podRegex); err != nil {
			fmt.Printf("Error getting pods by regex: %s (%v,%+v)\n", err, err, err)
			cancel()
		} else {
			currentlyTappedPods = matchingPods
		}

		nodeToTappedPodIPMap, err := getNodeHostToTappedPodIpsMap(currentlyTappedPods)
		if err != nil {
			fmt.Printf("Error building node to ips map: %s (%v,%+v)\n", err, err, err)
			cancel()
		}

		if err := createMizuTappers(ctx, kubernetesProvider, nodeToTappedPodIPMap, tappingOptions); err != nil {
			fmt.Printf("Error updating daemonset: %s (%v,%+v)\n", err, err, err)
			cancel()
		}
	}
	restartTappersDebouncer := debounce.NewDebouncer(updateTappersDelay, restartTappers)

	for {
		select {
		case newTarget := <-added:
			fmt.Printf("+%s\n", newTarget.Name)

		case removedTarget := <-removed:
			fmt.Printf("-%s\n", removedTarget.Name)
			restartTappersDebouncer.SetOn()

		case modifiedTarget := <-modified:
			// Act only if the modified pod has already obtained an IP address.
			// After filtering for IPs, on a normal pod restart this includes the following events:
			// - Pod deletion
			// - Pod reaches start state
			// - Pod reaches ready state
			// Ready/unready transitions might also trigger this event.
			if modifiedTarget.Status.PodIP != "" {
				restartTappersDebouncer.SetOn()
			}

		case <-errorChan:
			// TODO: Does this also perform cleanup?
			cancel()

		case <-ctx.Done():
			return
		}
	}
}

func portForwardApiPod(ctx context.Context, kubernetesProvider *kubernetes.Provider, cancel context.CancelFunc, tappingOptions *MizuTapOptions) {
	podExactRegex := regexp.MustCompile(fmt.Sprintf("^%s$", mizu.AggregatorPodName))
	added, modified, removed, errorChan := kubernetes.FilteredWatch(ctx, kubernetesProvider.GetPodWatcher(ctx, mizu.ResourcesNamespace), podExactRegex)
	isPodReady := false
	var portForward *kubernetes.PortForward
	for {
		select {
		case <-added:
			continue
		case <-removed:
			fmt.Printf("%s removed\n", mizu.AggregatorPodName)
			cancel()
			return
		case modifiedPod := <-modified:
			if modifiedPod.Status.Phase == "Running" && !isPodReady {
				isPodReady = true
				var err error
				portForward, err = kubernetes.NewPortForward(kubernetesProvider, mizu.ResourcesNamespace, mizu.AggregatorPodName, tappingOptions.GuiPort, tappingOptions.MizuPodPort, cancel)
				fmt.Printf("Web interface is now available at http://localhost:%d\n", tappingOptions.GuiPort)
				if err != nil {
					fmt.Printf("error forwarding port to pod %s\n", err)
					cancel()
				}
			}

		case <-time.After(25 * time.Second):
			if !isPodReady {
				fmt.Printf("error: %s pod was not ready in time", mizu.AggregatorPodName)
				cancel()
			}

		case <-errorChan:
			cancel()

		case <-ctx.Done():
			if portForward != nil {
				portForward.Stop()
			}
			return
		}
	}
}

func createRBACIfNecessary(ctx context.Context, kubernetesProvider *kubernetes.Provider) bool {
	mizuRBACExists, err := kubernetesProvider.DoesMizuRBACExist(ctx, mizu.ResourcesNamespace)
	if err != nil {
		fmt.Printf("warning: could not ensure mizu rbac resources exist %v\n", err)
		return false
	}
	if !mizuRBACExists {
		var versionString = mizu.Version
		if mizu.GitCommitHash != "" {
			versionString += "-" + mizu.GitCommitHash
		}
		err := kubernetesProvider.CreateMizuRBAC(ctx, mizu.ResourcesNamespace, versionString)
		if err != nil {
			fmt.Printf("warning: could not create mizu rbac resources %v\n", err)
			return false
		}
	}
	return true
}

func getNodeHostToTappedPodIpsMap(tappedPods []core.Pod) (map[string][]string, error) {
	nodeToTappedPodIPMap := make(map[string][]string, 0)
	for _, pod := range tappedPods {
		existingList := nodeToTappedPodIPMap[pod.Spec.NodeName]
		if existingList == nil {
			nodeToTappedPodIPMap[pod.Spec.NodeName] = []string{pod.Status.PodIP}
		} else {
			nodeToTappedPodIPMap[pod.Spec.NodeName] = append(nodeToTappedPodIPMap[pod.Spec.NodeName], pod.Status.PodIP)
		}
	}
	return nodeToTappedPodIPMap, nil
}

func waitForFinish(ctx context.Context, cancel context.CancelFunc) {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	// block until ctx cancel is called or termination signal is received
	select {
	case <-ctx.Done():
		break
	case <-sigChan:
		cancel()
	}
}

func syncApiStatus(ctx context.Context, cancel context.CancelFunc, tappingOptions *MizuTapOptions) {
	controlSocket, err := mizu.CreateControlSocket(fmt.Sprintf("ws://localhost:%d/ws", tappingOptions.GuiPort))
	if err != nil {
		fmt.Printf("error establishing control socket connection %s\n", err)
		cancel()
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
			err = controlSocket.SendNewTappedPodsListMessage(currentlyTappedPods)
			if err != nil {
				fmt.Printf("error Sending message via control socket %s\n", err)
			}
			time.Sleep(10 * time.Second)
		}
	}

}

func getNamespace(tappingOptions *MizuTapOptions, kubernetesProvider *kubernetes.Provider) string {
	if tappingOptions.AllNamespaces {
		return mizu.K8sAllNamespaces
	} else if len(tappingOptions.Namespace) > 0 {
		return tappingOptions.Namespace
	} else {
		return kubernetesProvider.CurrentNamespace()
	}
}
