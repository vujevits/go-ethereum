// Copyright 2017 The go-ethereum Authors
// This file is part of go-ethereum.
//
// go-ethereum is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// go-ethereum is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with go-ethereum. If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/log"
)

type testFile struct {
	filePath string
	content  string
	file     *os.File
}

// TestCLISwarmUp tests that running 'swarm up' makes the resulting file
// available from all nodes via the HTTP API
func TestCLISwarmFs(t *testing.T) {
	log.Info("starting 3 node cluster")
	cluster := newTestCluster(t, 3)
	//defer cluster.Shutdown()

	// create a tmp file
	mountPoint, err := ioutil.TempDir("", "swarm-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(mountPoint)
	handlingNode := cluster.Nodes[0]
	mhash := doUploadFile(t, handlingNode)

	mount := runSwarm(t, []string{
		"fs",
		"mount",
		"--ipcpath", filepath.Join(handlingNode.Dir, handlingNode.IpcPath),
		mhash,
		mountPoint,
	}...)
	mount.ExpectExit()

	filesToAssert := []*testFile{}

	dirPath, err := createDirInDir(mountPoint, "testSubDir")
	if err != nil {
		t.Fatal(err)
	}
	dirPath2, err := createDirInDir(dirPath, "AnotherTestSubDir")

	dummyContent := "somerandomtestcontentthatshouldbeasserted"
	for _, d := range []string{dirPath, dirPath2} {
		for _, entry := range []string{"f1.tmp", "f2.tmp"} {
			tFile, err := createTestFileInPath(d, entry, dummyContent)
			if err != nil {
				t.Fatal(err)
			}

			filesToAssert = append(filesToAssert, tFile)
		}
	}
	if len(filesToAssert) != 4 {
		t.Fatalf("should have 4 files to assert now, got %d", len(filesToAssert))
	}
	hashRegexp := `[a-f\d]{64}`
	lsMounts := runSwarm(t, []string{
		"fs",
		"unmount",
		"--ipcpath", filepath.Join(handlingNode.Dir, handlingNode.IpcPath),
		mountPoint,
	}...)
	_, matches := lsMounts.ExpectRegexp(hashRegexp)
	lsMounts.ExpectExit()
	hash := matches[0]
	if hash == mhash {
		t.Fatal("this should not be equal")
	}
	//check that there's nothing in the mount folder

	files, err := ioutil.ReadDir(mountPoint)
	if err != nil {
		t.Fatal(err)
	}

	if len(files) > 0 {
		t.Fatal("there shouldn't be anything here")
	}

	//remount, check files

	newMount := runSwarm(t, []string{
		"fs",
		"mount",
		"--ipcpath", filepath.Join(handlingNode.Dir, handlingNode.IpcPath),
		hash, // the latest hash
		mountPoint,
	}...)
	newMount.ExpectExit()

}

func doUploadFile(t *testing.T, node *testNode) string {
	// create a tmp file
	tmp, err := ioutil.TempFile("", "swarm-test")
	if err != nil {
		t.Fatal(err)
	}
	defer tmp.Close()
	defer os.Remove(tmp.Name())

	// write data to file
	data := "randomdata"
	_, err = io.WriteString(tmp, data)
	if err != nil {
		t.Fatal(err)
	}

	hashRegexp := `[a-f\d]{64}`

	flags := []string{
		"--bzzapi", node.URL,
		"up",
		tmp.Name()}

	log.Info(fmt.Sprintf("uploading file with 'swarm up'"))
	up := runSwarm(t, flags...)
	_, matches := up.ExpectRegexp(hashRegexp)
	up.ExpectExit()
	hash := matches[0]
	log.Info("file uploaded", "hash", hash)
	return hash

}

func createDirInDir(createInDir string, dirToCreate string) (string, error) {
	fullpath := filepath.Join(createInDir, dirToCreate)
	err := os.MkdirAll(fullpath, 0777)
	if err != nil {
		return "", err
	}
	return fullpath, nil
}

func createTestFileInPath(dir, filename, content string) (*testFile, error) {
	tFile := &testFile{}
	filePath := filepath.Join(dir, filename)
	if file, err := os.Create(filePath); err == nil {
		tFile.file = file
		tFile.content = content
		tFile.filePath = filePath

		_, err = io.WriteString(file, content)
		if err != nil {
			return nil, err
		}
	}

	return tFile, nil
}
