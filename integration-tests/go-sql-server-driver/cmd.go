// Copyright 2022 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

var DoltPath string

func init() {
	var err error
	DoltPath, err = exec.LookPath("dolt")
	if err != nil {
		panic(fmt.Sprintf("did not find dolt binary: %v", err.Error()))
	}
}

// DoltUser is an abstraction for a user account that calls `dolt` CLI
// commands. All of our dolt binary invocations are done through DoltUser.
//
// For our purposes, it does the following:
// * owns a tmpdir, to which it sets DOLT_ROOT_PATH when invoking dolt.
// * some initial dolt global config,
//   - user.name
//   - user.email
//   - metrics.disabled = true
//
// * can create repo stores, which will be a tmpdir to store a repo and/or subrepos.
type DoltUser struct {
	tmpdir string
}

func NewDoltUser() (DoltUser, error) {
	tmpdir, err := os.MkdirTemp("", "go-sql-server-dirver-")
	if err != nil {
		return DoltUser{}, err
	}
	res := DoltUser{tmpdir}
	err = res.DoltExec("config", "--global", "--add", "metrics.disabled", "true")
	if err != nil {
		return DoltUser{}, err
	}
	err = res.DoltExec("config", "--global", "--add", "user.name", "Bats Tests")
	if err != nil {
		return DoltUser{}, err
	}
	err = res.DoltExec("config", "--global", "--add", "user.email", "bats@email.fake")
	if err != nil {
		return DoltUser{}, err
	}
	return res, nil
}

func (u DoltUser) DoltCmd(args ...string) *exec.Cmd {
	cmd := exec.Command(DoltPath, args...)
	cmd.Dir = u.tmpdir
	cmd.Env = append(os.Environ(), "DOLT_ROOT_PATH="+u.tmpdir)
	return cmd
}

func (u DoltUser) DoltExec(args ...string) error {
	cmd := u.DoltCmd(args...)
	return cmd.Run()
}

func (u DoltUser) MakeRepoStore() (RepoStore, error) {
	tmpdir, err := os.MkdirTemp(u.tmpdir, "repo-store-")
	if err != nil {
		return RepoStore{}, err
	}
	return RepoStore{u, tmpdir}, nil
}

type RepoStore struct {
	user DoltUser
	dir  string
}

func (rs RepoStore) MakeRepo(name string) (Repo, error) {
	path := filepath.Join(rs.dir, name)
	err := os.Mkdir(path, 0750)
	if err != nil {
		return Repo{}, err
	}
	ret := Repo{rs.user, path}
	err = ret.DoltExec("init")
	if err != nil {
		return Repo{}, err
	}
	return ret, nil
}

func (rs RepoStore) DoltCmd(args ...string) *exec.Cmd {
	cmd := rs.user.DoltCmd(args...)
	cmd.Dir = rs.dir
	return cmd
}

func (rs RepoStore) WriteFile(path string, contents string) error {
	path = filepath.Join(rs.dir, path)
	d := filepath.Dir(path)
	err := os.MkdirAll(d, 0750)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(contents), 0550)
}

type Repo struct {
	user DoltUser
	dir  string
}

func (r Repo) DoltCmd(args ...string) *exec.Cmd {
	cmd := r.user.DoltCmd(args...)
	cmd.Dir = r.dir
	return cmd
}

func (r Repo) DoltExec(args ...string) error {
	cmd := r.DoltCmd(args...)
	err := cmd.Start()
	if err != nil {
		return err
	}
	return cmd.Wait()
}

func (r Repo) WriteFile(path string, contents string) error {
	path = filepath.Join(r.dir, path)
	d := filepath.Dir(path)
	err := os.MkdirAll(d, 0750)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(contents), 0550)
}

func (r Repo) CreateRemote(name, url string) error {
	cmd := r.DoltCmd("remote", "add", name, url)
	return cmd.Run()
}

type SqlServer struct {
	Done        chan struct{}
	Cmd         *exec.Cmd
	Port        int
	Output      *bytes.Buffer
	RecreateCmd func(args ...string) *exec.Cmd
}

type SqlServerOpt func(s *SqlServer)

func WithArgs(args ...string) SqlServerOpt {
	return func(s *SqlServer) {
		s.Cmd.Args = append(s.Cmd.Args, args...)
	}
}

func WithPort(port int) SqlServerOpt {
	return func(s *SqlServer) {
		s.Port = port
	}
}

type DoltCmdable interface {
	DoltCmd(...string) *exec.Cmd
}

func StartSqlServer(dc DoltCmdable, opts ...SqlServerOpt) (*SqlServer, error) {
	cmd := dc.DoltCmd("sql-server")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = cmd.Stdout
	output := new(bytes.Buffer)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		io.Copy(io.MultiWriter(os.Stdout, output), stdout)
	}()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	ret := &SqlServer{
		Done:   done,
		Cmd:    cmd,
		Port:   3306,
		Output: output,
		RecreateCmd: func(args ...string) *exec.Cmd {
			return dc.DoltCmd(args...)
		},
	}
	for _, o := range opts {
		o(ret)
	}
	err = ret.Cmd.Start()
	if err != nil {
		return nil, err
	}
	return ret, nil
}

func (r Repo) StartSqlServer(opts ...SqlServerOpt) (*SqlServer, error) {
	return StartSqlServer(r, opts...)
}

func (s *SqlServer) ErrorStop() error {
	<-s.Done
	return s.Cmd.Wait()
}

func (s *SqlServer) GracefulStop() error {
	err := s.Cmd.Process.Signal(syscall.SIGTERM)
	if err != nil {
		return err
	}
	<-s.Done
	return s.Cmd.Wait()
}

func (s *SqlServer) Restart(newargs *[]string) error {
	err := s.GracefulStop()
	if err != nil {
		return err
	}
	args := s.Cmd.Args[1:]
	if newargs != nil {
		args = append([]string{"sql-server"}, (*newargs)...)
	}
	s.Cmd = s.RecreateCmd(args...)
	stdout, err := s.Cmd.StdoutPipe()
	if err != nil {
		return err
	}
	s.Cmd.Stderr = s.Cmd.Stdout
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		io.Copy(io.MultiWriter(os.Stdout, s.Output), stdout)
	}()
	s.Done = make(chan struct{})
	go func() {
		wg.Wait()
		close(s.Done)
	}()
	return s.Cmd.Start()
}

func (s *SqlServer) DB(dbname string) (*sql.DB, error) {
	db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(127.0.0.1:%d)/%s", s.Port, dbname))
	if err != nil {
		return nil, err
	}
	for i := 0; i < 50; i++ {
		err = db.Ping()
		if err == nil {
			return db, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		return nil, err
	}
	return db, nil
}
