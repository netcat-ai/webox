package ilink

import (
	"errors"
	"fmt"
	"strings"
)

const imageItemType = 2

func (server *Server) materializeInboundImage(roomID, messageID string) (string, error) {
	if server.media == nil {
		return "", errors.New("shared media store is unavailable")
	}
	media, err := server.messages.ReadImage(roomID, messageID)
	if err != nil {
		return "", fmt.Errorf("read WeChat image: %w", err)
	}
	if media == nil {
		return "", errors.New("WeChat image is not available yet")
	}
	sharedPath, err := server.media.WriteInbox(roomID, messageID, media.Data, media.ContentType)
	if err != nil {
		return "", fmt.Errorf("write WeChat image to shared directory: %w", err)
	}
	return sharedPath, nil
}

func outboundImagePath(item map[string]any) (string, error) {
	imageItem, ok := item["image_item"].(map[string]any)
	if !ok {
		return "", errors.New("image_item must be an object")
	}
	sharedPath := strings.TrimSpace(stringValue(imageItem["shared_path"]))
	if sharedPath == "" {
		return "", errors.New("image_item.shared_path is required")
	}
	return sharedPath, nil
}
