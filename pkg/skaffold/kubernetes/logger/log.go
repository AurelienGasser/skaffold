/*
Copyright 2019 The Skaffold Authors

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

package logger

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"

	eventV2 "github.com/GoogleContainerTools/skaffold/pkg/skaffold/event/v2"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/kubectl"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/kubernetes"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/output"
	latestV1 "github.com/GoogleContainerTools/skaffold/pkg/skaffold/schema/latest/v1"
	"github.com/GoogleContainerTools/skaffold/pkg/skaffold/tag"
)

// LogAggregator aggregates the logs for all the deployed pods.
type LogAggregator struct {
	output      io.Writer
	kubectlcli  *kubectl.CLI
	config      Config
	podWatcher  kubernetes.PodWatcher
	colorPicker kubernetes.ColorPicker

	muted             int32
	sinceTime         time.Time
	events            chan kubernetes.PodEvent
	trackedContainers trackedContainers
	outputLock        sync.Mutex
}

type Config interface {
	PipelineForImage(imageName string) (latestV1.Pipeline, bool)
	DefaultPipeline() latestV1.Pipeline
}

// NewLogAggregator creates a new LogAggregator for a given output.
func NewLogAggregator(out io.Writer, cli *kubectl.CLI, imageNames []string, podSelector kubernetes.PodSelector, config Config) *LogAggregator {
	return &LogAggregator{
		output:      out,
		kubectlcli:  cli,
		config:      config,
		podWatcher:  kubernetes.NewPodWatcher(podSelector),
		colorPicker: kubernetes.NewColorPicker(imageNames),
		events:      make(chan kubernetes.PodEvent),
	}
}

func (a *LogAggregator) SetSince(t time.Time) {
	if a == nil {
		// Logs are not activated.
		return
	}

	a.sinceTime = t
}

// Start starts a logger that listens to pods and tail their logs
// if they are matched by the `podSelector`.
func (a *LogAggregator) Start(ctx context.Context, namespaces []string) error {
	if a == nil {
		// Logs are not activated.
		return nil
	}

	a.podWatcher.Register(a.events)
	stopWatcher, err := a.podWatcher.Start(namespaces)
	if err != nil {
		return err
	}

	go func() {
		defer stopWatcher()

		for {
			select {
			case <-ctx.Done():
				return
			case evt, ok := <-a.events:
				if !ok {
					return
				}

				// TODO(dgageot): Add EphemeralContainerStatuses
				pod := evt.Pod
				for _, c := range append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...) {
					if c.ContainerID == "" {
						if c.State.Waiting != nil && c.State.Waiting.Message != "" {
							output.Red.Fprintln(a.output, c.State.Waiting.Message)
						}
						continue
					}

					if !a.trackedContainers.add(c.ContainerID) {
						go a.streamContainerLogs(ctx, pod, c)
					}
				}
			}
		}
	}()

	return nil
}

// Stop stops the logger.
func (a *LogAggregator) Stop() {
	if a == nil {
		// Logs are not activated.
		return
	}

	close(a.events)
}

func sinceSeconds(d time.Duration) int64 {
	since := int64((d + 999*time.Millisecond).Truncate(1 * time.Second).Seconds())
	if since != 0 {
		return since
	}

	// 0 means all the logs. So we ask for the logs since 1s.
	return 1
}

func (a *LogAggregator) streamContainerLogs(ctx context.Context, pod *v1.Pod, container v1.ContainerStatus) {
	logrus.Infof("Streaming logs from pod: %s container: %s", pod.Name, container.Name)

	// In theory, it's more precise to use --since-time='' but there can be a time
	// difference between the user's machine and the server.
	// So we use --since=Xs and round up to the nearest second to not lose any log.
	sinceSeconds := fmt.Sprintf("--since=%ds", sinceSeconds(time.Since(a.sinceTime)))

	tr, tw := io.Pipe()
	go func() {
		if err := a.kubectlcli.Run(ctx, nil, tw, "logs", sinceSeconds, "-f", pod.Name, "-c", container.Name, "--namespace", pod.Namespace); err != nil {
			// Don't print errors if the user interrupted the logs
			// or if the logs were interrupted because of a configuration change
			if ctx.Err() != context.Canceled {
				logrus.Warn(err)
			}
		}
		_ = tw.Close()
	}()

	headerColor := a.colorPicker.Pick(pod)
	prefix := a.prefix(pod, container)
	if err := a.streamRequest(ctx, headerColor, prefix, pod.Name, container.Name, tr); err != nil {
		logrus.Errorf("streaming request %s", err)
	}
}

func (a *LogAggregator) printLogLine(headerColor output.Color, prefix, text string) {
	if !a.IsMuted() {
		a.outputLock.Lock()

		headerColor.Fprintf(a.output, "%s ", prefix)
		fmt.Fprint(a.output, text)

		a.outputLock.Unlock()
	}
}

func (a *LogAggregator) prefix(pod *v1.Pod, container v1.ContainerStatus) string {
	var c latestV1.Pipeline
	var present bool
	for _, container := range pod.Spec.Containers {
		if c, present = a.config.PipelineForImage(tag.StripTag(container.Image)); present {
			break
		}
	}
	if !present {
		c = a.config.DefaultPipeline()
	}
	switch c.Deploy.Logs.Prefix {
	case "auto":
		if pod.Name != container.Name {
			return podAndContainerPrefix(pod, container)
		}
		return autoPrefix(pod, container)
	case "container":
		return containerPrefix(container)
	case "podAndContainer":
		return podAndContainerPrefix(pod, container)
	case "none":
		return ""
	default:
		panic("unsupported prefix: " + c.Deploy.Logs.Prefix)
	}
}

func autoPrefix(pod *v1.Pod, container v1.ContainerStatus) string {
	if pod.Name != container.Name {
		return fmt.Sprintf("[%s %s]", pod.Name, container.Name)
	}
	return fmt.Sprintf("[%s]", container.Name)
}

func containerPrefix(container v1.ContainerStatus) string {
	return fmt.Sprintf("[%s]", container.Name)
}

func podAndContainerPrefix(pod *v1.Pod, container v1.ContainerStatus) string {
	return fmt.Sprintf("[%s %s]", pod.Name, container.Name)
}

func (a *LogAggregator) streamRequest(ctx context.Context, headerColor output.Color, prefix, podName, containerName string, rc io.Reader) error {
	r := bufio.NewReader(rc)
	for {
		select {
		case <-ctx.Done():
			logrus.Infof("%s interrupted", prefix)
			return nil
		default:
			// Read up to newline
			line, err := r.ReadString('\n')
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return fmt.Errorf("reading bytes from log stream: %w", err)
			}

			a.printLogLine(headerColor, prefix, line)
			eventV2.ApplicationLog(podName, containerName, line)
		}
	}
}

// Mute mutes the logs.
func (a *LogAggregator) Mute() {
	if a == nil {
		// Logs are not activated.
		return
	}

	atomic.StoreInt32(&a.muted, 1)
}

// Unmute unmutes the logs.
func (a *LogAggregator) Unmute() {
	if a == nil {
		// Logs are not activated.
		return
	}

	atomic.StoreInt32(&a.muted, 0)
}

// IsMuted says if the logs are to be muted.
func (a *LogAggregator) IsMuted() bool {
	return atomic.LoadInt32(&a.muted) == 1
}

type trackedContainers struct {
	sync.Mutex
	ids map[string]bool
}

// add adds a containerID to be tracked. Return true if the container
// was already tracked.
func (t *trackedContainers) add(id string) bool {
	t.Lock()
	alreadyTracked := t.ids[id]
	if t.ids == nil {
		t.ids = map[string]bool{}
	}
	t.ids[id] = true
	t.Unlock()

	return alreadyTracked
}
