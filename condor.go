//
// condor-launcher launches jobs on an HTCondor cluster.
//
// This service connects to an AMQP broker's "jobs" exchange and waits for
// messages sent with the "jobs.launches" key. It then turns the request
// into an iplant.cmd, config, job, and irods_config file in /tmp/<user>/<UUID>
// and calls out to condor_submit to submit the job to the cluster.
//
// Since it launches jobs by executing the condor_submit command it shouldn't
// run inside a Docker container. Our Condor cluster is moderately large and
// requires a lot of ports to be opened up, which doesn't play nicely with
// Docker.
//
// Required configuration keys are:
//   amqp.uri
//   irods.user
//   irods.pass
//   irods.host
//   irods.port
//   irods.base
//   irods.resc
//   irods.zone
//   condor.condor_config
//   condor.path_env_var
//   condor.log_path
//   condor.request_disk
//   porklock.image
//   porklock.tag
//
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"text/template"
	"time"

	"github.com/cyverse-de/configurate"
	"github.com/cyverse-de/logcabin"
	"github.com/cyverse-de/messaging"
	"github.com/cyverse-de/model"
	"github.com/cyverse-de/version"

	"github.com/spf13/viper"
	"github.com/streadway/amqp"
)

// CondorLauncher contains the condor-launcher application state.
type CondorLauncher struct {
	cfg *viper.Viper
}

// New returns a new *CondorLauncher
func New(c *viper.Viper) *CondorLauncher {
	return &CondorLauncher{
		cfg: c,
	}
}

// GenerateCondorSubmit returns a string (or error) containing the contents
// of what should go into an HTCondor submission file.
func (cl *CondorLauncher) GenerateCondorSubmit(submission *model.Job) (string, error) {
	tmpl := `universe = vanilla
executable = /usr/local/bin/road-runner
rank = mips
arguments = --config config --job job
output = script-output.log
error = script-error.log
log = condor.log{{if .Group}}
accounting_group = {{.Group}}
accounting_group_user = {{.Submitter}}{{end}}
request_disk = {{.RequestDisk}}
+IpcUuid = "{{.InvocationID}}"
+IpcJobId = "generated_script"
+IpcUsername = "{{.Submitter}}"
+IpcUserGroups = {{.FormatUserGroups}}
concurrency_limits = {{.UserIDForSubmission}}
{{with $x := index .Steps 0}}+IpcExe = "{{$x.Component.Name}}"{{end}}
{{with $x := index .Steps 0}}+IpcExePath = "{{$x.Component.Location}}"{{end}}
should_transfer_files = YES
transfer_input_files = irods-config,iplant.cmd,config,job
transfer_output_files = logs/de-transfer-trigger.log,logs/logs-stdout-output,logs/logs-stderr-output
when_to_transfer_output = ON_EXIT_OR_EVICT
notification = NEVER
queue
`
	t, err := template.New("condor_submit").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buffer bytes.Buffer
	err = t.Execute(&buffer, submission)
	if err != nil {
		return "", err
	}
	return buffer.String(), err
}

type scriptable struct {
	model.Job
	DC []model.VolumesFrom
	CI []model.ContainerImage
}

// GenerateJobConfig creates a string containing the config that gets passed
// into the job.
func (cl *CondorLauncher) GenerateJobConfig() (string, error) {
	tmpl := `amqp:
  uri: {{.GetString "amqp.uri"}}
irods:
  base: "{{.GetString "irods.base"}}"
porklock:
  image: "{{.GetString "porklock.image"}}"
  tag: "{{.GetString "porklock.tag"}}"
condor:
  filter_files: "{{.GetString "condor.filter_files"}}"`
	t, err := template.New("job_config").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buffer bytes.Buffer
	err = t.Execute(&buffer, cl.cfg)
	if err != nil {
		return "", err
	}
	return buffer.String(), nil
}

type irodsconfig struct {
	IRODSHost string
	IRODSPort string
	IRODSUser string
	IRODSPass string
	IRODSZone string
	IRODSBase string
	IRODSResc string
}

// GenerateIRODSConfig returns the contents of the irods-config file as a string.
func (cl *CondorLauncher) GenerateIRODSConfig() (string, error) {
	tmpl := `porklock.irods-host = {{.IRODSHost}}
porklock.irods-port = {{.IRODSPort}}
porklock.irods-user = {{.IRODSUser}}
porklock.irods-pass = {{.IRODSPass}}
porklock.irods-home = {{.IRODSBase}}
porklock.irods-zone = {{.IRODSZone}}
porklock.irods-resc = {{.IRODSResc}}
`
	t, err := template.New("irods_config").Parse(tmpl)
	if err != nil {
		return "", err
	}
	irodsHost := cl.cfg.GetString("irods.host")
	irodsPort := cl.cfg.GetString("irods.port")
	irodsUser := cl.cfg.GetString("irods.user")
	irodsPass := cl.cfg.GetString("irods.pass")
	irodsBase := cl.cfg.GetString("irods.base")
	irodsResc := cl.cfg.GetString("irods.resc")
	irodsZone := cl.cfg.GetString("irods.zone")
	c := &irodsconfig{
		IRODSHost: irodsHost,
		IRODSPort: irodsPort,
		IRODSUser: irodsUser,
		IRODSPass: irodsPass,
		IRODSBase: irodsBase,
		IRODSResc: irodsResc,
		IRODSZone: irodsZone,
	}
	var buffer bytes.Buffer
	err = t.Execute(&buffer, c)
	if err != nil {
		return "", err
	}
	return buffer.String(), err
}

// CreateSubmissionDirectory creates a directory for a submission and returns the path to it as a string.
func (cl *CondorLauncher) CreateSubmissionDirectory(s *model.Job) (string, error) {
	dirPath := s.CondorLogDirectory()
	if path.Base(dirPath) != "logs" {
		dirPath = path.Join(dirPath, "logs")
	}
	err := os.MkdirAll(dirPath, 0755)
	if err != nil {
		return "", err
	}
	return dirPath, err
}

// CreateSubmissionFiles creates the iplant.cmd file inside the
// directory designated by 'dir'. The return values are the path to the iplant.cmd
// file, and any errors, in that order.
func (cl *CondorLauncher) CreateSubmissionFiles(dir string, s *model.Job) (string, string, string, error) {
	cmdContents, err := cl.GenerateCondorSubmit(s)
	if err != nil {
		return "", "", "", err
	}

	jobConfigContents, err := cl.GenerateJobConfig()
	if err != nil {
		return "", "", "", err
	}

	jobContents, err := json.Marshal(s)
	if err != nil {
		return "", "", "", err
	}

	irodsContents, err := cl.GenerateIRODSConfig()
	if err != nil {
		return "", "", "", err
	}

	cmdPath := path.Join(dir, "iplant.cmd")
	configPath := path.Join(dir, "config")
	jobPath := path.Join(dir, "job")
	irodsPath := path.Join(dir, "irods-config")

	err = ioutil.WriteFile(cmdPath, []byte(cmdContents), 0644)
	if err != nil {
		return "", "", "", nil
	}

	err = ioutil.WriteFile(configPath, []byte(jobConfigContents), 0644)
	if err != nil {
		return "", "", "", nil
	}

	err = ioutil.WriteFile(jobPath, []byte(jobContents), 0644)
	if err != nil {
		return "", "", "", nil
	}

	err = ioutil.WriteFile(irodsPath, []byte(irodsContents), 0644)

	return cmdPath, configPath, jobPath, err
}

func (cl *CondorLauncher) submit(cmdPath string, s *model.Job) (string, error) {
	csPath, err := exec.LookPath("condor_submit")
	if err != nil {
		return "", err
	}

	if !path.IsAbs(csPath) {
		csPath, err = filepath.Abs(csPath)
		if err != nil {
			return "", err
		}
	}

	cmd := exec.Command(csPath, cmdPath)
	cmd.Dir = path.Dir(cmdPath)
	pathEnv := cl.cfg.GetString("condor.path_env_var")
	condorCfg := cl.cfg.GetString("condor.condor_config")

	cmd.Env = []string{
		fmt.Sprintf("PATH=%s", pathEnv),
		fmt.Sprintf("CONDOR_CONFIG=%s", condorCfg),
	}

	output, err := cmd.CombinedOutput()
	logcabin.Info.Printf("Output of condor_submit:\n%s\n", output)
	if err != nil {
		return "", err
	}

	logcabin.Info.Printf("Extracted ID: %s\n", string(model.ExtractJobID(output)))

	return string(model.ExtractJobID(output)), err
}

func (cl *CondorLauncher) launch(s *model.Job) (string, error) {
	sdir, err := cl.CreateSubmissionDirectory(s)
	if err != nil {
		logcabin.Error.Printf("Error creating submission directory:\n%s\n", err)
		return "", err
	}

	cmd, _, _, err := cl.CreateSubmissionFiles(sdir, s)
	if err != nil {
		logcabin.Error.Printf("Error creating submission files:\n%s", err)
		return "", err
	}

	id, err := cl.submit(cmd, s)
	if err != nil {
		logcabin.Error.Printf("Error submitting job:\n%s", err)
		return "", err
	}

	logcabin.Info.Printf("Condor job id is %s\n", id)

	return id, err
}

func (cl *CondorLauncher) stop(s *model.Job) (string, error) {
	crPath, err := exec.LookPath("condor_rm")
	logcabin.Info.Printf("condor_rm found at %s", crPath)
	if err != nil {
		return "", err
	}

	if !path.IsAbs(crPath) {
		crPath, err = filepath.Abs(crPath)
		if err != nil {
			return "", err
		}
	}

	pathEnv := cl.cfg.GetString("condor.path_env_var")
	condorConfig := cl.cfg.GetString("condor.condor_config")

	cmd := exec.Command(crPath, s.CondorID)
	cmd.Env = []string{
		fmt.Sprintf("PATH=%s", pathEnv),
		fmt.Sprintf("CONDOR_CONFIG=%s", condorConfig),
	}

	output, err := cmd.CombinedOutput()
	logcabin.Info.Printf("condor_rm output for job %s:\n%s\n", s.CondorID, string(output))
	if err != nil {
		return "", err
	}

	return string(output), err
}

// startHeldTicker starts up the code that periodically fires and clean up held
// jobs
func (cl *CondorLauncher) startHeldTicker(client *messaging.Client) (*time.Ticker, error) {
	d, err := time.ParseDuration("30s")
	if err != nil {
		return nil, err
	}
	t := time.NewTicker(d)
	go func(t *time.Ticker, client *messaging.Client) {
		for {
			select {
			case <-t.C:
				cl.killHeldJobs(client)
			}
		}
	}(t, client)
	return t, nil
}

func main() {
	var (
		cfgPath     = flag.String("config", "", "Path to the config file. Required.")
		showVersion = flag.Bool("version", false, "Print the version information")
	)

	flag.Parse()

	logcabin.Init("condor-launcher", "condor-launcher")

	if *showVersion {
		version.AppVersion()
		os.Exit(0)
	}

	if *cfgPath == "" {
		fmt.Println("Error: --config must be set.")
		flag.PrintDefaults()
		os.Exit(-1)
	}

	cfg, err := configurate.InitDefaults(*cfgPath, configurate.JobServicesDefaults)
	if err != nil {
		logcabin.Error.Fatal(err)
	}
	logcabin.Info.Println("Done reading config.")

	launcher := New(cfg)

	uri := cfg.GetString("amqp.uri")

	client, err := messaging.NewClient(uri, true)
	if err != nil {
		logcabin.Error.Fatal(err)
	}
	defer client.Close()

	client.SetupPublishing(messaging.JobsExchange)

	go client.Listen()

	ticker, err := launcher.startHeldTicker(client)
	if err != nil {
		logcabin.Error.Fatal(err)
	}
	logcabin.Info.Printf("Started up the held state ticker: %#v", ticker)

	launcher.RegisterStopHandler(client)

	// Accept and handle messages sent out with the jobs.launches routing key.
	client.AddConsumer(messaging.JobsExchange, "topic", "condor_launches", messaging.LaunchesKey, func(d amqp.Delivery) {
		body := d.Body
		d.Ack(false)

		req := messaging.JobRequest{}
		err := json.Unmarshal(body, &req)
		if err != nil {
			logcabin.Error.Print(err)
			logcabin.Error.Print(string(body[:]))
			return
		}

		if req.Job.RequestDisk == "" {
			req.Job.RequestDisk = "0"
		}

		switch req.Command {
		case messaging.Launch:
			jobID, err := launcher.launch(req.Job)
			if err != nil {
				logcabin.Error.Print(err)
				err = client.PublishJobUpdate(&messaging.UpdateMessage{
					Job:     req.Job,
					State:   messaging.FailedState,
					Message: fmt.Sprintf("condor-launcher failed to launch job:\n %s", err),
				})
				if err != nil {
					logcabin.Error.Print(err)
				}
			} else {
				logcabin.Info.Printf("Launched Condor ID %s", jobID)
				err = client.PublishJobUpdate(&messaging.UpdateMessage{
					Job:     req.Job,
					State:   messaging.SubmittedState,
					Message: fmt.Sprintf("Launched Condor ID %s", jobID),
				})
				if err != nil {
					logcabin.Error.Print(err)
				}
			}
		}
	})

	spin := make(chan int)
	<-spin
}
