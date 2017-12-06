package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/stvp/rollbar"

	"github.com/docker/docker/daemon/logger"
	docker "github.com/fsouza/go-dockerclient"
)

type Monitor struct {
	client *docker.Client

	envs       map[string]map[string]string
	logDrivers map[string]string

	agentId      string
	agentImage   string
	agentVersion string

	amiId        string
	az           string
	instanceId   string
	instanceType string
	region       string

	dockerDriver        string
	dockerServerVersion string
	ecsAgentImage       string
	kernelVersion       string
	convoxVersion       string

	lock    sync.Mutex
	lines   map[string][][]byte
	loggers map[string]logger.Logger
}

func NewMonitor() *Monitor {
	fmt.Printf("NewMonitor at=start client_id=%s region=%s kinesis=%s log_group=%s\n", os.Getenv("CLIENT_ID"), os.Getenv("AWS_REGION"), os.Getenv("KINESIS"), os.Getenv("LOG_GROUP"))

	client, err := docker.NewClient(os.Getenv("DOCKER_HOST"))
	if err != nil {
		fmt.Printf("NewMonitor docker.NewClient endpoint=%s err=%q\n", os.Getenv("DOCKER_HOST"), err)
	}

	info, err := client.Info()
	if err != nil {
		fmt.Printf("NewMonitor client.Info err=%q\n", err)
	}

	img, err := GetECSAgentImage(client)
	if err != nil {
		fmt.Printf("NewMonitor GetECSAgentImage err=%q\n", err)
	}

	m := &Monitor{
		client: client,

		envs:       make(map[string]map[string]string),
		logDrivers: make(map[string]string),

		agentId:      "unknown",          // updated during handleRunning
		agentImage:   "convox/agent:dev", // updated during handleRunning
		agentVersion: "dev",              // updated during handleRunning

		amiId:        "ami-dev",
		az:           "us-dev-1b",
		instanceId:   "i-dev",
		instanceType: "d1.dev",
		region:       "us-dev-1",

		dockerDriver:        info.Get("Driver"),
		dockerServerVersion: info.Get("ServerVersion"),
		ecsAgentImage:       img,
		kernelVersion:       info.Get("KernelVersion"),

		lines:   make(map[string][][]byte),
		loggers: make(map[string]logger.Logger),
	}

	cfg := ec2metadata.Config{}

	if os.Getenv("EC2_METADATA_ENDPOINT") != "" {
		cfg.Endpoint = aws.String(os.Getenv("EC2_METADATA_ENDPOINT"))
	}

	svc := ec2metadata.New(&cfg)

	if os.Getenv("DEVELOPMENT") != "true" && svc.Available() {
		m.amiId, _ = svc.GetMetadata("ami-id")
		m.az, _ = svc.GetMetadata("placement/availability-zone")
		m.instanceId, _ = svc.GetMetadata("instance-id")
		m.instanceType, _ = svc.GetMetadata("instance-type")
		m.region, _ = svc.Region()
	}

	fmt.Printf("NewMonitor az=%s instanceId=%s instanceType=%s region=%s agentImage=%s amiId=%s dockerServerVersion=%s ecsAgentImage=%s kernelVersion=%s\n",
		m.az, m.instanceId, m.instanceType, m.region,
		m.agentImage, m.amiId, m.dockerServerVersion, m.ecsAgentImage, m.kernelVersion,
	)

	return m
}

// Write event to app CloudWatch Log Group and Kinesis stream
func (m *Monitor) logAppEvent(id, message string) {
	// append syslog-ish prefix:
	// agent:0.66/i-553ffcd2 Starting hello-world process 977a93d4d48e

	msg := fmt.Sprintf("agent:%s/%s %s", m.agentVersion, m.instanceId, message)

	ts := time.Now()

	if awslogger, ok := m.loggers[id]; ok {
		awslogger.Log(&logger.Message{
			ContainerID: id,
			Line:        []byte(msg),
			Timestamp:   ts,
		})
	}

	if stream, ok := m.envs[id]["KINESIS"]; ok {
		m.addLine(stream, []byte(fmt.Sprintf("%s %s", ts.Format("2006-01-02 15:04:05"), msg))) // add timestamp to kinesis for legacy purposes
	}
}

// logSystem write event to stdout and convox CloudWatch Log Group, prefixed with an instance id
func (m *Monitor) logSystemf(format string, a ...interface{}) {
	line := fmt.Sprintf(format, a...)
	l := fmt.Sprintf("agent:%s/%s %s", m.agentVersion, m.instanceId, line)

	fmt.Println(l)

	id := m.agentId

	if awslogger, ok := m.loggers[id]; ok {
		awslogger.Log(&logger.Message{
			ContainerID: id,
			Line:        []byte(l),
			Timestamp:   time.Now(),
		})
	}
}

func GetECSAgentImage(client *docker.Client) (string, error) {
	containers, err := client.ListContainers(docker.ListContainersOptions{})

	if err != nil {
		return "error", err
	}

	for _, c := range containers {
		if strings.HasPrefix(c.Image, "amazon/amazon-ecs-agent") {
			ic, err := client.InspectContainer(c.ID)

			if err != nil {
				return "unknown", err
			}

			return ic.Image[0:12], nil
		}
	}

	return "notfound", nil
}

func (m *Monitor) ReportError(err error) {
	m.logSystemf("monitor ReportError err=%q", err)

	rollbar.Token = "ca01e8fc13ed4aa893f7a0300d94f2e1"

	extraData := map[string]string{
		"agentId":    m.agentId,
		"agentImage": m.agentImage,

		"clientId": os.Getenv("CLIENT_ID"),

		"amiId":        m.amiId,
		"az":           m.az,
		"instanceId":   m.instanceId,
		"instanceType": m.instanceType,
		"region":       m.region,

		"dockerDriver":        m.dockerDriver,
		"dockerServerVersion": m.dockerServerVersion,
		"ecsAgentImage":       m.ecsAgentImage,
		"kernelVersion":       m.kernelVersion,
	}
	extraField := &rollbar.Field{"env", extraData}

	rollbar.ErrorWithStackSkip(rollbar.CRIT, err, 1, extraField)
}

func (m *Monitor) SetUnhealthy(system string, reason error) {
	metric := ucfirst(system) + "Error" // DockerError or DmesgError
	m.logSystemf("%s ok=false count#%s err=%q", system, metric, reason)
	m.ReportError(reason)

	AutoScaling := autoscaling.New(&aws.Config{})

	_, err := AutoScaling.SetInstanceHealth(&autoscaling.SetInstanceHealthInput{
		HealthStatus:             aws.String("Unhealthy"),
		InstanceId:               aws.String(m.instanceId),
		ShouldRespectGracePeriod: aws.Bool(true),
	})
	if err != nil {
		m.logSystemf("monitor AutoScaling.SetInstanceHealth count#AutoScalingSetInstanceHealthError=1 err=%q", err)
	} else {
		// log for humans
		m.logSystemf("who=\"convox/agent\" what=\"marked instance %s unhealthy\" why=\"%s %s\"", m.instanceId, system, reason)
	}

	// Dump dmesg to convox log stream and rollbar
	out, err := exec.Command("dmesg").CombinedOutput()
	if err != nil {
		m.ReportError(err)
	} else {
		m.ReportError(errors.New(string(out)))
	}
}

func ucfirst(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[0:1]) + strings.ToLower(s[1:len(s)])
}
