package ilink

import (
	"errors"
	"fmt"
	"strings"

	"github.com/netcat-ai/webox/internal/wechatdb"
)

const imageItemType = 2
const fileItemType = 4

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

func outboundImagePath(item map[string]any) (string, error) {
	return outboundMediaPath(item, "image_item")
}

func outboundFilePath(item map[string]any) (string, error) {
	return outboundMediaPath(item, "file_item")
}

func outboundMediaPath(item map[string]any, key string) (string, error) {
	body, ok := item[key].(map[string]any)
	if !ok {
		return "", fmt.Errorf("%s must be an object", key)
	}
	sharedPath := strings.TrimSpace(stringValue(body["shared_path"]))
	if sharedPath == "" {
		return "", fmt.Errorf("%s.shared_path is required", key)
	}
	return sharedPath, nil
}
