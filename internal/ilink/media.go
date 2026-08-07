package ilink

import (
	"errors"
	"fmt"

	"github.com/netcat-ai/webox/internal/wechatdb"
)

var errInboundMediaNotReady = errors.New("inbound media is not ready")

func (server *Server) materializeInboundImage(roomID, messageID string) (string, error) {
	if server.media == nil {
		return "", errors.New("shared media store is unavailable")
	}
	media, err := server.messages.ReadImage(roomID, messageID)
	if err != nil {
		return "", fmt.Errorf("%w: read WeChat image: %v", errInboundMediaNotReady, err)
	}
	if media == nil {
		return "", errInboundMediaNotReady
	}
	sharedPath, err := server.media.WriteInbox(roomID, messageID, media.Data, media.ContentType)
	if err != nil {
		return "", fmt.Errorf("write WeChat image to shared directory: %w", err)
	}
	return sharedPath, nil
}

func (server *Server) materializeInboundFile(roomID, messageID string) (*wechatdb.LocalFile, string, error) {
	if server.media == nil {
		return nil, "", errors.New("shared media store is unavailable")
	}
	file, err := server.messages.ReadFile(roomID, messageID)
	if err != nil {
		return nil, "", fmt.Errorf("%w: read WeChat file: %v", errInboundMediaNotReady, err)
	}
	if file == nil {
		return nil, "", errInboundMediaNotReady
	}
	sharedPath, err := server.media.CopyInboxFile(roomID, messageID, file.Filename, file.Path)
	if err != nil {
		return nil, "", fmt.Errorf("write WeChat file to shared directory: %w", err)
	}
	return file, sharedPath, nil
}
