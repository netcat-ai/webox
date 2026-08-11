package ilink

import (
	"errors"
	"fmt"
	"time"

	"github.com/netcat-ai/webox/internal/wechatdb"
	"github.com/netcat-ai/webox/wecom"
)

var errInboundMediaNotReady = errors.New("inbound media is not ready")

func inboundMediaWaitExpired(message wecom.Message, now time.Time) bool {
	return message.MsgTime <= 0 || !now.Before(time.UnixMilli(message.MsgTime).Add(inboundMediaWait))
}

func (server *Server) materializeInboundImage(roomID string, message wecom.Message) (string, error) {
	if server.media == nil {
		return "", errors.New("shared media store is unavailable")
	}
	media, err := server.messages.ReadImage(roomID, message.Sequence, time.UnixMilli(message.MsgTime).Unix(), message.Image.MD5Sum)
	if err != nil {
		return "", fmt.Errorf("read WeChat image: %w", err)
	}
	if media == nil {
		return "", errInboundMediaNotReady
	}
	sharedPath, err := server.media.WriteInbox(roomID, message.MsgID, media.Data, media.ContentType)
	if err != nil {
		return "", fmt.Errorf("write WeChat image to shared directory: %w", err)
	}
	return sharedPath, nil
}

func (server *Server) materializeInboundFile(roomID string, message wecom.Message) (*wechatdb.LocalFile, string, error) {
	if server.media == nil {
		return nil, "", errors.New("shared media store is unavailable")
	}
	file, err := server.messages.ReadFile(roomID, message.Sequence, time.UnixMilli(message.MsgTime).Unix(), message.File.FileName)
	if err != nil {
		return nil, "", fmt.Errorf("read WeChat file: %w", err)
	}
	if file == nil {
		return nil, "", errInboundMediaNotReady
	}
	sharedPath, err := server.media.CopyInboxFile(roomID, message.MsgID, file.Filename, file.Path)
	if err != nil {
		return nil, "", fmt.Errorf("write WeChat file to shared directory: %w", err)
	}
	return file, sharedPath, nil
}
