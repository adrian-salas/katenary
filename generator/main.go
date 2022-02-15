package generator

import (
	"fmt"
	"io/ioutil"
	"katenary/compose"
	"katenary/helm"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"errors"

	"github.com/google/shlex"
)

var servicesMap = make(map[string]int)
var serviceWaiters = make(map[string][]chan int)
var locker = &sync.Mutex{}

const (
	ICON_PACKAGE = "📦"
	ICON_SERVICE = "🔌"
	ICON_SECRET  = "🔏"
	ICON_CONF    = "📝"
	ICON_STORE   = "⚡"
	ICON_INGRESS = "🌐"
)

const (
	RELEASE_NAME = helm.RELEASE_NAME
)

// Values is kept in memory to create a values.yaml file.
var Values = helm.Values{}

var VolumeValues = make(map[string]map[string]map[string]interface{}) //TODO: use a type

var dependScript = `
OK=0
echo "Checking __service__ port"
while [ $OK != 1 ]; do
    echo -n "."
    nc -z ` + RELEASE_NAME + `-__service__ __port__ 2>&1 >/dev/null && OK=1 || sleep 1
done
echo
echo "Done"
`

// Create a Deployment for a given compose.Service. It returns a list of objects: a Deployment and a possible Service (kubernetes represnetation as maps).
func CreateReplicaObject(name string, s *compose.Service) chan interface{} {
	ret := make(chan interface{}, len(s.Ports)+len(s.Expose)+1)
	go parseService(name, s, ret)
	return ret
}

// This function will try to yied deployment and services based on a service from the compose file structure.
func parseService(name string, s *compose.Service, ret chan interface{}) {
	Magenta(ICON_PACKAGE+" Generating deployment for ", name)

	o := helm.NewDeployment(name)
	container := helm.NewContainer(name, s.Image, s.Environment, s.Labels)

	// check the image, and make it "variable" in values.yaml
	container.Image = "{{ .Values." + name + ".image }}"
	Values[name] = map[string]interface{}{
		"image": s.Image,
	}

	// prepare cm and secrets
	prepareEnvFromFiles(name, s, container, ret)

	// manage the healthcheck property, if any
	prepareProbes(name, s, container)
	// manage ports
	generateContainerPorts(s, name, container)

	// Set the container to the deployment
	o.Spec.Template.Spec.Containers = []*helm.Container{container}

	// Prepare volumes
	o.Spec.Template.Spec.Volumes = prepareVolumes(name, s, container, ret)

	// Add selectors
	selectors := buildSelector(name, s)
	o.Spec.Selector = map[string]interface{}{
		"matchLabels": selectors,
	}
	o.Spec.Template.Metadata.Labels = selectors

	// Now, for "depends_on" section, it's a bit tricky to get dependencies, see the function below.
	o.Spec.Template.Spec.InitContainers = prepareInitContainers(name, s, container)

	// Then, create Services and possible Ingresses for ingress labels, "ports" and "expose" section
	if len(s.Ports) > 0 || len(s.Expose) > 0 {
		for _, s := range generateServicesAndIngresses(name, s) {
			ret <- s
		}
	}

	// Special case, it there is no "ports", so there is no associated services...
	// But... some other deployment can wait for it, so we alert that this deployment hasn't got any
	// associated service.
	if len(s.Ports) == 0 {
		// alert any current or **future** waiters that this service is not exposed
		go func() {
			for {
				select {
				case <-time.Tick(1 * time.Millisecond):
					locker.Lock()
					for _, c := range serviceWaiters[name] {
						c <- -1
						close(c)
					}
					locker.Unlock()
				}
			}
		}()
	}

	// add the volumes in Values
	if len(VolumeValues[name]) > 0 {
		locker.Lock()
		Values[name]["persistence"] = VolumeValues[name]
		locker.Unlock()
	}

	// the deployment is ready, give it
	ret <- o

	// and then, we can say that it's the end
	ret <- nil
}

// Create a service (k8s).
func generateServicesAndIngresses(name string, s *compose.Service) []interface{} {

	ret := make([]interface{}, 0) // can handle helm.Service or helm.Ingress
	Magenta(ICON_SERVICE+" Generating service for ", name)
	ks := helm.NewService(name)

	for i, p := range s.Ports {
		port := strings.Split(p, ":")
		src, _ := strconv.Atoi(port[0])
		target := src
		if len(port) > 1 {
			target, _ = strconv.Atoi(port[1])
		}
		ks.Spec.Ports = append(ks.Spec.Ports, helm.NewServicePort(target, target))
		if i == 0 {
			detected(name, target)
		}
	}
	ks.Spec.Selector = buildSelector(name, s)

	ret = append(ret, ks)
	if v, ok := s.Labels[helm.LABEL_INGRESS]; ok {
		port, err := strconv.Atoi(v)
		if err != nil {
			log.Fatalf("The given port \"%v\" as ingress port in \"%s\" service is not an integer\n", v, name)
		}
		Cyanf(ICON_INGRESS+" Create an ingress for port %d on %s service\n", port, name)
		ing := createIngress(name, port, s)
		ret = append(ret, ing)
	}

	if len(s.Expose) > 0 {
		Magenta(ICON_SERVICE+" Generating service for ", name+"-external")
		ks := helm.NewService(name + "-external")
		ks.Spec.Type = "NodePort"
		for _, p := range s.Expose {
			ks.Spec.Ports = append(ks.Spec.Ports, helm.NewServicePort(p, p))
		}
		ks.Spec.Selector = buildSelector(name, s)
		ret = append(ret, ks)
	}

	return ret
}

// Create an ingress.
func createIngress(name string, port int, s *compose.Service) *helm.Ingress {
	ingress := helm.NewIngress(name)
	Values[name]["ingress"] = map[string]interface{}{
		"class":   "nginx",
		"host":    name + "." + helm.Appname + ".tld",
		"enabled": false,
	}
	ingress.Spec.Rules = []helm.IngressRule{
		{
			Host: fmt.Sprintf("{{ .Values.%s.ingress.host }}", name),
			Http: helm.IngressHttp{
				Paths: []helm.IngressPath{{
					Path:     "/",
					PathType: "Prefix",
					Backend: helm.IngressBackend{
						Service: helm.IngressService{
							Name: RELEASE_NAME + "-" + name,
							Port: map[string]interface{}{
								"number": port,
							},
						},
					},
				}},
			},
		},
	}
	ingress.SetIngressClass(name)

	return ingress
}

// This function is called when a possible service is detected, it append the port in a map to make others
// to be able to get the service name. It also try to send the data to any "waiter" for this service.
func detected(name string, port int) {
	locker.Lock()
	defer locker.Unlock()
	if _, ok := servicesMap[name]; ok {
		return
	}
	servicesMap[name] = port
	go func() {
		locker.Lock()
		defer locker.Unlock()
		if cx, ok := serviceWaiters[name]; ok {
			for _, c := range cx {
				c <- port
			}
		}
	}()
}

func getPort(name string) (int, error) {
	if v, ok := servicesMap[name]; ok {
		return v, nil
	}
	return -1, errors.New("Not found")
}

// Waits for a service to be discovered. Sometimes, a deployment depends on another one. See the detected() function.
func waitPort(name string) chan int {
	locker.Lock()
	defer locker.Unlock()
	c := make(chan int, 0)
	serviceWaiters[name] = append(serviceWaiters[name], c)
	go func() {
		locker.Lock()
		defer locker.Unlock()
		if v, ok := servicesMap[name]; ok {
			c <- v
		}
	}()
	return c
}

// Build the selector for the service.
func buildSelector(name string, s *compose.Service) map[string]string {
	return map[string]string{
		"katenary.io/component": name,
		"katenary.io/release":   RELEASE_NAME,
	}
}

// buildCMFromPath generates a ConfigMap from a path.
func buildCMFromPath(path string) *helm.ConfigMap {
	stat, err := os.Stat(path)
	if err != nil {
		return nil
	}

	files := make(map[string]string, 0)
	if stat.IsDir() {
		found, _ := filepath.Glob(path + "/*")
		for _, f := range found {
			if s, err := os.Stat(f); err != nil || s.IsDir() {
				if err != nil {
					fmt.Fprintf(os.Stderr, "An error occured reading volume path %s\n", err.Error())
				} else {
					ActivateColors = true
					Yellowf("Warning, %s is a directory, at this time we only "+
						"can create configmap for first level file list\n", f)
					ActivateColors = false
				}
				continue
			}
			_, filename := filepath.Split(f)
			c, _ := ioutil.ReadFile(f)
			files[filename] = string(c)
		}
	}

	cm := helm.NewConfigMap("")
	cm.Data = files
	return cm
}

// generateContainerPorts add the container ports of a service.
func generateContainerPorts(s *compose.Service, name string, container *helm.Container) {

	exists := make(map[int]string)
	for _, port := range s.Ports {
		_p := strings.Split(port, ":")
		port = _p[0]
		if len(_p) > 1 {
			port = _p[1]
		}
		portNumber, _ := strconv.Atoi(port)
		portName := name
		for _, n := range exists {
			if name == n {
				portName = fmt.Sprintf("%s-%d", name, portNumber)
			}
		}
		container.Ports = append(container.Ports, &helm.ContainerPort{
			Name:          portName,
			ContainerPort: portNumber,
		})
		exists[portNumber] = name
	}

	// manage the "expose" section to be a NodePort in Kubernetes
	for _, port := range s.Expose {
		if _, exist := exists[port]; exist {
			continue
		}
		container.Ports = append(container.Ports, &helm.ContainerPort{
			Name:          name,
			ContainerPort: port,
		})
	}
}

// prepareVolumes add the volumes of a service.
func prepareVolumes(name string, s *compose.Service, container *helm.Container, ret chan interface{}) []map[string]interface{} {

	volumes := make([]map[string]interface{}, 0)
	mountPoints := make([]interface{}, 0)
	configMapsVolumes := make([]string, 0)
	if v, ok := s.Labels[helm.LABEL_VOL_CM]; ok {
		configMapsVolumes = strings.Split(v, ",")
	}
	for _, volume := range s.Volumes {
		parts := strings.Split(volume, ":")
		volname := parts[0]
		volepath := parts[1]

		isCM := false
		for _, cmVol := range configMapsVolumes {
			cmVol = strings.TrimSpace(cmVol)
			if volname == cmVol {
				isCM = true
				break
			}
		}

		if !isCM && (strings.HasPrefix(volname, ".") || strings.HasPrefix(volname, "/")) {
			// local volume cannt be mounted
			ActivateColors = true
			Redf("You cannot, at this time, have local volume in %s deployment\n", name)
			ActivateColors = false
			continue
		}
		if isCM {
			// the volume is a path and it's explicitally asked to be a configmap in labels
			cm := buildCMFromPath(volname)
			volname = strings.Replace(volname, "./", "", 1)
			volname = strings.ReplaceAll(volname, ".", "-")
			cm.K8sBase.Metadata.Name = RELEASE_NAME + "-" + volname + "-" + name
			// build a configmap from the volume path
			volumes = append(volumes, map[string]interface{}{
				"name": volname,
				"configMap": map[string]string{
					"name": cm.K8sBase.Metadata.Name,
				},
			})
			mountPoints = append(mountPoints, map[string]interface{}{
				"name":      volname,
				"mountPath": volepath,
			})
			ret <- cm
		} else {

			// rmove minus sign from volume name
			volname = strings.ReplaceAll(volname, "-", "")

			pvc := helm.NewPVC(name, volname)
			volumes = append(volumes, map[string]interface{}{
				"name": volname,
				"persistentVolumeClaim": map[string]string{
					"claimName": RELEASE_NAME + "-" + volname,
				},
			})
			mountPoints = append(mountPoints, map[string]interface{}{
				"name":      volname,
				"mountPath": volepath,
			})

			Yellow(ICON_STORE+" Generate volume values for ", volname, " in deployment ", name)
			locker.Lock()
			if _, ok := VolumeValues[name]; !ok {
				VolumeValues[name] = make(map[string]map[string]interface{})
			}
			VolumeValues[name][volname] = map[string]interface{}{
				"enabled":  false,
				"capacity": "1Gi",
			}
			locker.Unlock()
			ret <- pvc
		}
	}
	container.VolumeMounts = mountPoints
	return volumes
}

// prepareInitContainers add the init containers of a service.
func prepareInitContainers(name string, s *compose.Service, container *helm.Container) []*helm.Container {

	// We need to detect others services, but we probably not have parsed them yet, so
	// we will wait for them for a while.
	initContainers := make([]*helm.Container, 0)
	for _, dp := range s.DependsOn {
		c := helm.NewContainer("check-"+dp, "busybox", nil, s.Labels)
		command := strings.ReplaceAll(strings.TrimSpace(dependScript), "__service__", dp)

		foundPort := -1
		if defaultPort, err := getPort(dp); err != nil {
			// BUG: Sometimes the chan remains opened
			foundPort = <-waitPort(dp)
		} else {
			foundPort = defaultPort
		}
		if foundPort == -1 {
			log.Fatalf(
				"ERROR, the %s service is waiting for %s port number, "+
					"but it is never discovered. You must declare at least one port in "+
					"the \"ports\" section of the service in the docker-compose file",
				name,
				dp,
			)
		}
		command = strings.ReplaceAll(command, "__port__", strconv.Itoa(foundPort))

		c.Command = []string{
			"sh",
			"-c",
			command,
		}
		initContainers = append(initContainers, c)
	}
	return initContainers
}

// prepareProbes generate http/tcp/command probes for a service.
func prepareProbes(name string, s *compose.Service, container *helm.Container) {

	// manage the healthcheck property, if any
	if s.HealthCheck != nil {
		if s.HealthCheck.Interval == "" {
			s.HealthCheck.Interval = "10s"
		}
		interval, err := time.ParseDuration(s.HealthCheck.Interval)

		if err != nil {
			log.Fatal(err)
		}
		if s.HealthCheck.StartPeriod == "" {
			s.HealthCheck.StartPeriod = "0s"
		}

		initialDelaySeconds, err := time.ParseDuration(s.HealthCheck.StartPeriod)
		if err != nil {
			log.Fatal(err)
		}

		probe := helm.NewProbe(int(interval.Seconds()), int(initialDelaySeconds.Seconds()), 1, s.HealthCheck.Retries)

		healthCheckLabel := s.Labels[helm.LABEL_HEALTHCHECK]

		if healthCheckLabel != "" {

			path := "/"
			port := 80

			u, err := url.Parse(healthCheckLabel)
			if err == nil {
				path = u.Path
				port, _ = strconv.Atoi(u.Port())
			} else {
				path = "/"
				port = 80
			}

			if strings.HasPrefix(healthCheckLabel, "http://") {
				probe.HttpGet = &helm.HttpGet{
					Path: path,
					Port: port,
				}
			} else if strings.HasPrefix(healthCheckLabel, "tcp://") {
				if err != nil {
					log.Fatal(err)
				}
				probe.TCP = &helm.TCP{
					Port: port,
				}
			} else {
				c, _ := shlex.Split(healthCheckLabel)
				probe.Exec = &helm.Exec{

					Command: c,
				}
			}
		} else if s.HealthCheck.Test[0] == "CMD" {
			probe.Exec = &helm.Exec{
				Command: s.HealthCheck.Test[1:],
			}
		}
		container.LivenessProbe = probe
	}
}

// prepareEnvFromFiles generate configMap or secrets from environment files.
func prepareEnvFromFiles(name string, s *compose.Service, container *helm.Container, ret chan interface{}) {

	// prepare secrets
	secretsFiles := make([]string, 0)
	if v, ok := s.Labels[helm.LABEL_ENV_SECRET]; ok {
		secretsFiles = strings.Split(v, ",")
	}

	// manage environment files (env_file in compose)
	for _, envfile := range s.EnvFiles {
		f := strings.ReplaceAll(envfile, "_", "-")
		f = strings.ReplaceAll(f, ".env", "")
		f = strings.ReplaceAll(f, ".", "")
		f = strings.ReplaceAll(f, "/", "")
		cf := f + "-" + name
		isSecret := false
		for _, s := range secretsFiles {
			if s == envfile {
				isSecret = true
			}
		}
		var store helm.InlineConfig
		if !isSecret {
			Bluef(ICON_CONF+" Generating configMap %s\n", cf)
			store = helm.NewConfigMap(cf)
		} else {
			Bluef(ICON_SECRET+" Generating secret %s\n", cf)
			store = helm.NewSecret(cf)
		}
		locker.Lock()
		if err := store.AddEnvFile(envfile, name, &Values); err != nil {
			ActivateColors = true
			Red(err.Error())
			ActivateColors = false
			os.Exit(2)
		}
		locker.Unlock()

		section := "configMapRef"
		if isSecret {
			section = "secretRef"
		}

		container.EnvFrom = append(container.EnvFrom, map[string]map[string]string{
			section: {
				"name": store.Metadata().Name,
			},
		})

		ret <- store
	}
}
