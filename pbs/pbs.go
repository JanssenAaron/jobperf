package pbs

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path"
	"strings"

	"github.com/clemsonciti/jobperf"
	"golang.org/x/crypto/ssh"
)

func IsAvailable() bool {
	p, err := exec.LookPath("qstat")
	// Real qstat is found in a folder with pbs in it:
	if err == nil && strings.Contains(p, "pbs") {
		slog.Debug("found qstat")
		return true
	}
	slog.Debug("failed to find qstat", "err", err)
	return false
}

type jobEngine struct {
	pbsBinPath string
}

func NewJobEngine() jobperf.JobEngine {
	var engine jobEngine
	qstatPath, err := exec.LookPath("qstat")
	if err != nil {
		slog.Error("failed to find pbs binary path. Defaulting to /opt/pbs/default/bin", "err", err)
		engine.pbsBinPath = "/opt/pbs/default/bin"
	} else {
		engine.pbsBinPath = path.Dir(qstatPath)
	}
	return engine
}

func (e jobEngine) GetJobByID(jobID string) (*jobperf.Job, error) {
	return e.qstatGetJob(jobID)
}

func (_ jobEngine) SelectJobIDs(q jobperf.JobQuery) ([]string, error) {
	return nil, nil
}

func (e jobEngine) NodeStatsSession(j *jobperf.Job, hostname string) (jobperf.NodeStatsSession, error) {
	var statsSession nodeStatsSession
	var err error
	statsSession.jobID = j.ID
	statsSession.hostname = hostname
	statsSession.sshConn, err = connectToNode(hostname, j.Owner)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to node %v with ssh: %w", hostname, err)
	}
	statsSession.sshSession, err = statsSession.sshConn.NewSession()
	if err != nil {
		return nil, fmt.Errorf("failed create session on node %v with ssh: %w", hostname, err)
	}
	statsSession.reqWriter, err = statsSession.sshSession.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("failed create req writer on node %v with ssh: %w", hostname, err)
	}
	statsSession.resReader, err = statsSession.sshSession.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed create res reader on node %v with ssh: %w", hostname, err)
	}
	ex, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("failed find jobperf: %w", err)
	}
	cmd := fmt.Sprintf("%s/pbs_attach -j %v %v -nodestats", e.pbsBinPath, j.ID, ex)
	err = statsSession.sshSession.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed start nodestats with cmd=%v on node %v with ssh: %w", cmd, hostname, err)
	}

	return &statsSession, nil
}

func (_ jobEngine) Warning() string {
	return ""
}

func (_ jobEngine) NodeStatsWarning() string {
	return ""
}

type nodeStatsSession struct {
	sshConn    *ssh.Client
	sshSession *ssh.Session
	reqWriter  io.Writer
	resReader  io.Reader
	hostname   string
	jobID      string
}

func (s *nodeStatsSession) RequestCPUStats() (*jobperf.NodeStatsCPUMem, error) {
	payload, err := json.Marshal(jobperf.NodeStatsCPUMemPBSPayload{
		JobID: s.jobID,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to write payload for pbs node stats request: %w", err)
	}
	err = json.NewEncoder(s.reqWriter).Encode(jobperf.NodeStatsRequest{
		RequestType: jobperf.NodeStatsRequestTypeSampleCPUMemPBS,
		Payload:     payload,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to encode request for pbs node stats request: %w", err)
	}
	var res jobperf.NodeStatsCPUMem
	err = json.NewDecoder(s.resReader).Decode(&res)
	if err != nil {
		return nil, fmt.Errorf("failed to decode cpu/mem response from node stats: %w", err)
	}
	res.Hostname = s.hostname
	return &res, nil
}

func (s *nodeStatsSession) RequestGPUStats() (*jobperf.NodeStatsGPU, error) {
	err := json.NewEncoder(s.reqWriter).Encode(jobperf.NodeStatsRequest{
		RequestType: jobperf.NodeStatsRequestTypeSampleGPUNvidia,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to encode gpu stats request for pbs node stats request: %w", err)
	}
	var res jobperf.NodeStatsGPU
	err = json.NewDecoder(s.resReader).Decode(&res)
	if err != nil {
		return nil, fmt.Errorf("failed to decode gpu response from node stats: %w", err)
	}
	for i := range res {
		res[i].Hostname = s.hostname
	}
	return &res, nil
}

func (s *nodeStatsSession) Close() error {
	// Ask nicely...
	err := json.NewEncoder(s.reqWriter).Encode(jobperf.NodeStatsRequest{
		RequestType: jobperf.NodeStatsRequestTypeExit,
	})
	if err != nil {
		return fmt.Errorf("failed to encode exit request for pbs node stats request: %w", err)
	}
	// Then clean up forcibly.
	s.sshSession.Close()
	s.sshConn.Close()
	return nil
}

func connectToNode(host string, username string) (*ssh.Client, error) {
	user, err := user.Lookup(username)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch user info for user %s: %w", username, err)
	}
	if user.HomeDir == "" {
		return nil, fmt.Errorf("user %s is missing home directory", username)
	}
	keyFiles := []string{"id_rsa", "id_ecdsa", "id_ecdsa_sk", "id_ed25519", "id_ed25519_sk", "id_dsa"}
	var key []byte
        var keyFilepath string
	for _, keyType := range keyFiles {
		keyFile := fmt.Sprintf("%s/.ssh/%s", user.HomeDir, keyType)
		key, err = os.ReadFile(keyFile)
		if err != nil {
			slog.Info("failed to read private key %s: %w", keyFile, err)
		} else {
                    keyFilepath = keyFile
                    break;
		}
	}
	if key == nil {
		return nil, fmt.Errorf("Failed to find ssh private key")
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("failed to parse private key %s: %w", keyFilepath, err)
	}

	config := &ssh.ClientConfig{
		User: username,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	client, err := ssh.Dial("tcp", host+":22", config)
	if err != nil {
		return nil, fmt.Errorf("failed to dial host %s: %w", host, err)
	}
	return client, nil
}
