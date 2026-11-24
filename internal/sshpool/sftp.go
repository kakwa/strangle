package sshpool

import (
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

const timeout = 30 * time.Second

func newSFTPClient(client *ssh.Client) (*sftp.Client, error) {
	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		return nil, err
	}
	return sftpClient, nil
}
