// Copyright (c) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package multidevice

import (
	"encoding/binary"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/RadicalApp/libsignal-protocol-go/ecc"
	waBinary "go.mau.fi/whatsmeow/binary"
)

type ReadReceipt struct {
	From       waBinary.FullJID
	Chat       *waBinary.FullJID
	Recipient  *waBinary.FullJID
	MessageIDs []string
	Timestamp  int64
}

func (cli *Client) handleReceipt(node *waBinary.Node) bool {
	if node.Tag != "receipt" {
		return false
	}

	if node.AttrGetter().OptionalString("type") == "read" {
		receipt, err := cli.parseReadReceipt(node)
		if err != nil {
			cli.Log.Warnln("Failed to parse read receipt:", err)
		} else {
			go cli.dispatchEvent(receipt)
		}
	}
	go cli.sendAck(node)
	return true
}

func (cli *Client) parseReadReceipt(node *waBinary.Node) (*ReadReceipt, error) {
	ag := node.AttrGetter()
	if ag.String("type") != "read" {
		return nil, nil
	}
	receipt := ReadReceipt{
		From:       ag.JID("from"),
		Recipient:  ag.OptionalJID("recipient"),
		Timestamp:  ag.Int64("t"),
	}
	if receipt.From.Server == waBinary.GroupServer {
		receipt.Chat = &receipt.From
		receipt.From = ag.JID("participant")
	}
	if !ag.OK() {
		return nil, fmt.Errorf("failed to parse read receipt attrs: %+v", ag.Errors)
	}

	receiptChildren := node.GetChildren()
	if len(receiptChildren) == 1 && receiptChildren[0].Tag == "list" {
		listChildren := receiptChildren[0].GetChildren()
		receipt.MessageIDs = make([]string, 0, len(listChildren))
		for _, item := range listChildren {
			if id, ok := item.Attrs["id"].(string); ok && item.Tag == "item" {
				receipt.MessageIDs = append(receipt.MessageIDs, id)
			}
		}
	} else {
		receipt.MessageIDs = []string{ag.String("id")}
	}
	return &receipt, nil
}

func (cli *Client) sendAck(node *waBinary.Node) {
	attrs := map[string]interface{}{
		"class": node.Tag,
		"id":    node.Attrs["id"],
	}
	attrs["to"] = node.Attrs["from"]
	if participant, ok := node.Attrs["participant"]; ok {
		attrs["participant"] = participant
	}
	if recipient, ok := node.Attrs["recipient"]; ok {
		attrs["recipient"] = recipient
	}
	if receiptType, ok := node.Attrs["type"]; node.Tag == "receipt" && ok {
		attrs["type"] = receiptType
	}
	err := cli.sendNode(waBinary.Node{
		Tag:   "ack",
		Attrs: attrs,
	})
	if err != nil {
		cli.Log.Warnfln("Failed to send acknowledgement for %s %s: %v", node.Tag, node.Attrs["id"], err)
	}
}

//func (cli *Client) ackMessage(info *MessageInfo) {
//	attrs := map[string]interface{}{
//		"class": "message",
//		"id":    info.ID,
//	}
//	if info.Chat != nil {
//		attrs["to"] = *info.Chat
//		// TODO is this really supposed to be the user instead of info.Participant?
//		attrs["participant"] = waBinary.NewADJID(cli.Session.ID.User, 0, 0)
//	} else {
//		attrs["to"] = waBinary.NewJID(cli.Session.ID.User, waBinary.UserServer)
//	}
//	err := cli.sendNode(waBinary.Node{
//		Tag:   "ack",
//		Attrs: attrs,
//	})
//	if err != nil {
//		cli.Log.Warnfln("Failed to send acknowledgement for %s: %v", info.ID, err)
//	}
//}

func (cli *Client) sendMessageReceipt(info *MessageInfo) {
	attrs := map[string]interface{}{
		"id": info.ID,
	}
	isFromMe := info.From.User == cli.Session.ID.User
	if isFromMe {
		attrs["type"] = "sender"
	} else {
		attrs["type"] = "inactive"
	}
	if info.Chat != nil {
		attrs["to"] = *info.Chat
		attrs["participant"] = info.From
	} else {
		attrs["to"] = info.From
		if isFromMe && info.Recipient != nil {
			attrs["recipient"] = *info.Recipient
		}
	}
	err := cli.sendNode(waBinary.Node{
		Tag:   "receipt",
		Attrs: attrs,
	})
	if err != nil {
		cli.Log.Warnfln("Failed to send receipt for %s: %v", info.ID, err)
	}
}

func (cli *Client) sendRetryReceipt(node *waBinary.Node) {
	id, _ := node.Attrs["id"].(string)

	cli.messageRetriesLock.Lock()
	cli.messageRetries[id]++
	retryCount := cli.messageRetries[id]
	cli.messageRetriesLock.Unlock()

	var registrationIDBytes [4]byte
	binary.BigEndian.PutUint32(registrationIDBytes[:], cli.Session.RegistrationID)
	attrs := map[string]interface{}{
		"id":   id,
		"type": "retry",
		"to":   node.Attrs["from"],
	}
	if recipient, ok := node.Attrs["recipient"]; ok {
		attrs["recipient"] = recipient
	}
	if participant, ok := node.Attrs["participant"]; ok {
		attrs["participant"] = participant
	}
	payload := waBinary.Node{
		Tag:   "receipt",
		Attrs: attrs,
		Content: []waBinary.Node{
			{Tag: "retry", Attrs: map[string]interface{}{
				"count": retryCount,
				"id":    id,
				"t":     node.Attrs["t"],
				"v":     1,
			}},
			{Tag: "registration", Content: registrationIDBytes[:]},
		},
	}
	if retryCount > 1 {
		keys := cli.Session.GetOrGenPreKeys(1)
		deviceIdentity, err := proto.Marshal(cli.Session.Account)
		if err != nil {
			cli.Log.Errorln("Failed to marshal account info:", err)
			return
		}
		payload.Content = append(payload.GetChildren(), waBinary.Node{
			Tag: "keys",
			Content: []waBinary.Node{
				{Tag: "type", Content: []byte{ecc.DjbType}},
				{Tag: "identity", Content: cli.Session.IdentityKey.Pub[:]},
				preKeyToNode(keys[0]),
				preKeyToNode(cli.Session.SignedPreKey),
				{Tag: "device-identity", Content: deviceIdentity},
			},
		})
	}
	err := cli.sendNode(payload)
	if err != nil {
		cli.Log.Errorfln("Failed to send retry receipt for %s: %v", id, err)
	}
}
