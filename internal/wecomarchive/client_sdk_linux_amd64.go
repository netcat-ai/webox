//go:build linux && amd64 && cgo

package wecomarchive

import (
	"github.com/NICEXAI/WeWorkFinanceSDK"
)

type sdkClient struct {
	client WeWorkFinanceSDK.Client
}

func OpenClient(corpID, secret, privateKey string) (Client, error) {
	client, err := WeWorkFinanceSDK.NewClient(corpID, secret, privateKey)
	if err != nil {
		return nil, err
	}
	return &sdkClient{client: client}, nil
}

func (client *sdkClient) GetChatData(sequence, limit uint64) ([]ChatData, error) {
	data, err := client.client.GetChatData(sequence, limit, "", "", 5)
	if err != nil {
		return nil, err
	}
	result := make([]ChatData, len(data))
	for index, item := range data {
		result[index] = ChatData{
			Sequence: item.Seq, MessageID: item.MsgId,
			EncryptedKey: item.EncryptRandomKey, EncryptedMessage: item.EncryptChatMsg,
		}
	}
	return result, nil
}

func (client *sdkClient) DecryptData(data ChatData) ([]byte, error) {
	message, err := client.client.DecryptData(data.EncryptedKey, data.EncryptedMessage, "")
	if err != nil {
		return nil, err
	}
	return message.GetRawChatMessage(), nil
}

func (client *sdkClient) GetMediaData(index, sdkFileID string) (*MediaData, error) {
	data, err := client.client.GetMediaData(index, sdkFileID, "", "", 5)
	if err != nil {
		return nil, err
	}
	return &MediaData{NextIndex: data.OutIndexBuf, Finished: data.IsFinish, Data: data.Data}, nil
}

func (client *sdkClient) Free() {
	client.client.Free()
}
