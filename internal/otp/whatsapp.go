// WhatsApp Cloud API sender — Meta's official channel for OTP delivery
// (https://developers.facebook.com/docs/whatsapp/cloud-api). Requires a
// WhatsApp Business Account, a registered sender number (phone_number_id),
// a permanent access token, and a Meta-approved AUTHENTICATION template —
// the code lands as a template parameter, not free text. If the template
// carries a copy-code button, pass its sub-type via buttonSubType so the
// code is bound to the button too (Meta rejects a missing/extra component).
package otp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// waEndpoint is the Graph API base — overridable for testability.
var waEndpoint = "https://graph.facebook.com/v21.0"

// waClient — a hung Meta call must not strand an OTP send goroutine.
var waClient = &http.Client{Timeout: 15 * time.Second}

// WhatsAppSender returns a SendFunc delivering codes through an
// authentication template. templateName/lang select the approved template
// (e.g. "oga9_otp", "en_US"); buttonSubType is "copy_code"|"url" when the
// template includes an OTP button, "" when it doesn't.
func WhatsAppSender(phoneNumberID, token, templateName, lang, buttonSubType string) SendFunc {
	return func(ctx context.Context, destination, code string) error {
		to := strings.NewReplacer("+", "", " ", "", "-", "").Replace(destination)
		components := []map[string]any{{
			"type":       "body",
			"parameters": []map[string]string{{"type": "text", "text": code}},
		}}
		if buttonSubType != "" {
			components = append(components, map[string]any{
				"type":       "button",
				"sub_type":   buttonSubType,
				"index":      "0",
				"parameters": []map[string]string{{"type": "text", "text": code}},
			})
		}
		payload, _ := json.Marshal(map[string]any{
			"messaging_product": "whatsapp",
			"to":                to,
			"type":              "template",
			"template": map[string]any{
				"name":       templateName,
				"language":   map[string]string{"code": lang},
				"components": components,
			},
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			waEndpoint+"/"+phoneNumberID+"/messages", bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := waClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			return fmt.Errorf("whatsapp api: %d", resp.StatusCode)
		}
		return nil
	}
}
