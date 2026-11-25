package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/raghavkhandelwal11/Siliconia_Backend/internal/store"
)

// Channel set for a template. In-app is always on; SMS is sent for every
// transactional template that lists it; WhatsApp and email follow the
// account's preferences (LLD 13).
type channels struct{ sms, whatsapp, email bool }

// templates lists each template's channels (TRS 6.17 event catalogue).
var templates = map[string]channels{
	"order_confirmed":         {sms: true, whatsapp: true, email: true},
	"seller_new_order":        {sms: true, whatsapp: true, email: true},
	"order_shipped":           {sms: true, whatsapp: true},
	"order_out_for_delivery":  {sms: true, whatsapp: true},
	"order_delivered":         {sms: true, whatsapp: true, email: true},
	"order_cancelled":         {sms: true, email: true},
	"order_rto":               {sms: true, email: true},
	"refund_processed":        {sms: true, email: true},
	"return_status":           {whatsapp: true, email: true},
	"seller_return_requested": {whatsapp: true, email: true},
	"issue_update":            {email: true},
	"approval_result":         {sms: true, email: true},
	"membership_renewed":      {sms: true, email: true},
	"cashout_paid":            {sms: true, email: true},
	"membership_predebit":     {sms: true, whatsapp: true, email: true},
	"membership_lapsed":       {sms: true, email: true},
	"electrons_available":     {whatsapp: true},
}

// Notice is one message for one account.
type Notice struct {
	Account  uuid.UUID
	Template string
	Title    string
	Body     string // short enough for SMS
	Link     string // path in the web app
}

type payload map[string]any

func (p payload) str(k string) string {
	switch v := p[k].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func (p payload) id(k string) (uuid.UUID, bool) {
	v, err := uuid.Parse(p.str(k))
	return v, err == nil
}

func (p payload) num(k string) int64 {
	switch v := p[k].(type) {
	case float64:
		return int64(v)
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

// rupees formats paise for messages: ₹1,23,456 (paise only when present).
func rupees(paise int64) string {
	r := strconv.FormatInt(paise/100, 10)
	if len(r) > 3 {
		head, tail := r[:len(r)-3], r[len(r)-3:]
		var parts []string
		for len(head) > 2 {
			parts = append([]string{head[len(head)-2:]}, parts...)
			head = head[:len(head)-2]
		}
		if head != "" {
			parts = append([]string{head}, parts...)
		}
		r = strings.Join(parts, ",") + "," + tail
	}
	if p := paise % 100; p != 0 {
		r += fmt.Sprintf(".%02d", p)
	}
	return "₹" + r
}

// route turns an outbox event into notices. Unknown topics produce none.
func route(ctx context.Context, q *store.Queries, topic string, raw []byte) ([]Notice, error) {
	var p payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, nil // not ours to judge; nothing to send
	}
	switch topic {
	case "order.confirmed":
		return orderConfirmed(ctx, q, p)
	case "suborder.status_changed":
		return subOrderChanged(ctx, q, p)
	case "refund.completed":
		return refundCompleted(ctx, q, p)
	case "return.status_changed":
		return returnChanged(p)
	case "issue.updated":
		return issueUpdated(p)
	case "account.approval_changed":
		return approvalChanged(p)
	case "cashout.finished":
		acc, ok := p.id("accountId")
		if !ok {
			return nil, nil
		}
		if p.str("status") == "PAID" {
			return []Notice{{Account: acc, Template: "cashout_paid", Title: "Cash-out paid",
				Body: "₹" + strconv.FormatInt(p.num("electrons"), 10) + " has been sent to your bank account.", Link: "/creator"}}, nil
		}
		return []Notice{{Account: acc, Template: "cashout_paid", Title: "Cash-out failed",
			Body: "Your bank transfer failed, so your Electrons are back in your balance. Check your bank details.", Link: "/creator"}}, nil
	case "membership.activated", "membership.predebit", "membership.lapsed", "electrons.lot_available":
		return m3Notice(topic, p)
	}
	return nil, nil
}

func orderConfirmed(ctx context.Context, q *store.Queries, p payload) ([]Notice, error) {
	orderID, ok := p.id("orderId")
	if !ok {
		return nil, nil
	}
	o, err := q.GetOrder(ctx, orderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	how := "Paid " + rupees(o.PayablePaise) + "."
	if o.PayMethod == "COD" {
		how = "Pay " + rupees(o.PayablePaise) + " on delivery."
	}
	out := []Notice{{Account: o.BuyerID, Template: "order_confirmed", Title: "Order " + o.Number + " confirmed",
		Body: "Your order " + o.Number + " is confirmed. " + how + " We'll tell you when it ships.", Link: "/account/orders/" + o.ID.String()}}
	subs, err := q.ListSubOrders(ctx, o.ID)
	if err != nil {
		return nil, err
	}
	for _, s := range subs {
		if s.ReplacesItemID != nil {
			continue
		}
		out = append(out, Notice{Account: s.SellerID, Template: "seller_new_order", Title: "New order " + o.Number,
			Body: "New order " + o.Number + " for " + rupees(s.SubtotalPaise) + ". Pack and ship it from Seller > Orders.",
			Link: "/seller/orders/" + s.ID.String()})
	}
	return out, nil
}

func subOrderChanged(ctx context.Context, q *store.Queries, p payload) ([]Notice, error) {
	subID, ok := p.id("subOrderId")
	if !ok {
		return nil, nil
	}
	d, err := q.GetSubOrderDetail(ctx, subID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	link := "/account/orders/" + d.OrderID.String()
	awb := ""
	if sh, err := q.GetForwardShipment(ctx, &subID); err == nil {
		awb = " (AWB " + sh.Awb + ")"
	}
	from := " from " + d.StoreName
	switch p.str("status") {
	case "SHIPPED":
		if p.str("tracking") != "" {
			return nil, nil // pickup and hub scans: the order was already reported as shipped
		}
		return []Notice{{Account: d.BuyerID, Template: "order_shipped", Title: "Order " + d.Number + " shipped",
			Body: "Your items" + from + " in order " + d.Number + " have shipped" + awb + ".", Link: link}}, nil
	case "OUT_FOR_DELIVERY":
		if p.str("tracking") == "FAILED_ATTEMPT" {
			return []Notice{{Account: d.BuyerID, Template: "order_out_for_delivery", Title: "Delivery attempt missed",
				Body: "The courier couldn't deliver order " + d.Number + " today. They'll try again.", Link: link}}, nil
		}
		return []Notice{{Account: d.BuyerID, Template: "order_out_for_delivery", Title: "Out for delivery today",
			Body: "Order " + d.Number + from + " is out for delivery today.", Link: link}}, nil
	case "DELIVERED":
		return []Notice{{Account: d.BuyerID, Template: "order_delivered", Title: "Order " + d.Number + " delivered",
			Body: "Order " + d.Number + from + " was delivered. Not right? Return or replace within 7 days.", Link: link}}, nil
	case "RTO":
		return []Notice{
			{Account: d.BuyerID, Template: "order_rto", Title: "Order " + d.Number + " returned to seller",
				Body: "The courier couldn't deliver order " + d.Number + " and returned it to the seller.", Link: link},
			{Account: d.SellerID, Template: "order_rto", Title: "Order " + d.Number + " coming back",
				Body: "Order " + d.Number + " could not be delivered and is returning to you.", Link: "/seller/orders/" + subID.String()},
		}, nil
	case "CANCELLED":
		reason := ""
		if d.CancelReason != nil {
			reason = " Reason: " + *d.CancelReason + "."
		}
		refund := ""
		if d.PayMethod == "ONLINE" {
			refund = " Your refund of " + rupees(d.SubtotalPaise) + " is on its way."
		}
		out := []Notice{{Account: d.BuyerID, Template: "order_cancelled", Title: "Order " + d.Number + " cancelled",
			Body: "Items" + from + " in order " + d.Number + " were cancelled." + reason + refund, Link: link}}
		if p.str("by") == "BUYER" {
			out = append(out, Notice{Account: d.SellerID, Template: "order_cancelled", Title: "Order " + d.Number + " cancelled by buyer",
				Body: "The buyer cancelled order " + d.Number + "." + reason + " Don't ship it.", Link: "/seller/orders/" + subID.String()})
		}
		return out, nil
	case "CONFIRMED":
		if p.str("replacement") == "true" {
			return []Notice{{Account: d.SellerID, Template: "seller_new_order", Title: "Replacement to ship for " + d.Number,
				Body: "A returned item from order " + d.Number + " needs a replacement. Ship it from Seller > Orders.",
				Link: "/seller/orders/" + subID.String()}}, nil
		}
	}
	return nil, nil
}

func refundCompleted(ctx context.Context, q *store.Queries, p payload) ([]Notice, error) {
	if p.str("status") != "REFUNDED" {
		return nil, nil
	}
	payID, ok := p.id("paymentId")
	if !ok {
		return nil, nil
	}
	pay, err := q.GetPayment(ctx, payID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if pay.AccountID == nil {
		return nil, nil
	}
	return []Notice{{Account: *pay.AccountID, Template: "refund_processed", Title: rupees(p.num("amountPaise")) + " refunded",
		Body: "We've refunded " + rupees(p.num("amountPaise")) + " to your original payment method. Banks take up to 5 working days to show it.",
		Link: "/account/orders"}}, nil
}

var returnWords = map[string]string{
	"REJECTED":            "wasn't approved by the seller",
	"PICKUP_SCHEDULED":    "is approved. A courier will collect the item",
	"PICKED_UP":           "has been picked up",
	"RECEIVED":            "reached the seller. Your replacement is being prepared",
	"REFUNDED":            "is complete and your refund is on its way",
	"REFUND_PENDING":      "is complete. We'll transfer your refund to your bank account",
	"REPLACEMENT_SHIPPED": "is complete and your replacement has shipped",
}

func returnChanged(p payload) ([]Notice, error) {
	buyer, ok1 := p.id("buyerId")
	seller, ok2 := p.id("sellerId")
	number, status := p.str("number"), p.str("status")
	if !ok1 || !ok2 {
		return nil, nil
	}
	if status == "REQUESTED" {
		return []Notice{{Account: seller, Template: "seller_return_requested", Title: "Return request " + number,
			Body: "A buyer asked to return an item (" + number + "). Approve or reject it in Seller > Returns.", Link: "/seller/returns"}}, nil
	}
	words, ok := returnWords[status]
	if !ok {
		return nil, nil
	}
	return []Notice{{Account: buyer, Template: "return_status", Title: "Return " + number,
		Body: "Your return " + number + " " + words + ".", Link: "/account/returns"}}, nil
}

func issueUpdated(p payload) ([]Notice, error) {
	buyer, ok := p.id("buyerId")
	if !ok || p.str("by") != "SUPPORT" {
		return nil, nil
	}
	id, number := p.str("issueId"), p.str("number")
	body := "Support replied to your issue " + number + "."
	switch p.str("status") {
	case "WAITING_CUSTOMER":
		body = "Support needs a reply from you on issue " + number + "."
	case "RESOLVED":
		body = "Support marked issue " + number + " as resolved. Reopen it if you still need help."
	}
	return []Notice{{Account: buyer, Template: "issue_update", Title: "Update on " + number, Body: body, Link: "/account/issues/" + id}}, nil
}

func approvalChanged(p payload) ([]Notice, error) {
	acc, ok := p.id("accountId")
	if !ok {
		return nil, nil
	}
	if role := p.str("role"); role == "CREATOR" || role == "PROMOTER" {
		home, name := "/creator", "creator"
		if role == "PROMOTER" {
			home, name = "/promoter", "promoter"
		}
		if p.str("approval") == "APPROVED" {
			return []Notice{{Account: acc, Template: "approval_result", Title: "You're approved as a " + name,
				Body: "Your Siliconics " + name + " application is approved. Open your dashboard to get started.", Link: home}}, nil
		}
		if p.str("approval") == "REJECTED" {
			return []Notice{{Account: acc, Template: "approval_result", Title: "Application not approved",
				Body: "Your " + name + " application needs changes. See the reason on your dashboard.", Link: home}}, nil
		}
		return nil, nil
	}
	switch p.str("approval") {
	case "APPROVED":
		return []Notice{{Account: acc, Template: "approval_result", Title: "You're approved to sell",
			Body: "Your Siliconics seller account is approved. Add your first product from the seller dashboard.", Link: "/seller"}}, nil
	case "REJECTED":
		return []Notice{{Account: acc, Template: "approval_result", Title: "Seller application not approved",
			Body: "Your seller application needs changes. See the reasons and re-apply from the seller dashboard.", Link: "/seller"}}, nil
	}
	return nil, nil
}

func m3Notice(topic string, p payload) ([]Notice, error) {
	acc, ok := p.id("accountId")
	if !ok {
		return nil, nil
	}
	switch topic {
	case "membership.activated":
		return []Notice{{Account: acc, Template: "membership_renewed", Title: "You're an Electra Member",
			Body: "Your Electra Membership is active for a year: member prices, 4x Electrons, cash on delivery and launch deals.", Link: "/membership"}}, nil
	case "membership.predebit":
		return []Notice{{Account: acc, Template: "membership_predebit", Title: "Membership renews soon",
			Body: "Your Electra Membership renews in 2 days for " + rupees(p.num("amountPaise")) + ". Turn off auto-renew on the membership page to stop it.", Link: "/membership"}}, nil
	case "membership.lapsed":
		return []Notice{{Account: acc, Template: "membership_lapsed", Title: "Membership ended",
			Body: "Your Electra Membership has ended. Renew any time to get member benefits back.", Link: "/membership"}}, nil
	case "electrons.lot_available":
		return []Notice{{Account: acc, Template: "electrons_available", Title: strconv.FormatInt(p.num("amount"), 10) + " Electrons available",
			Body: strconv.FormatInt(p.num("amount"), 10) + " Electrons are now ready to spend. 1 Electron = ₹1 off your next order.", Link: "/account/electrons"}}, nil
	}
	return nil, nil
}
