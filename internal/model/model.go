// Package model holds the shapes that cross the API boundary.
package model

import (
	"time"

	"github.com/OatApisit/billsplit-api/internal/money"
)

// User is a LINE user known to this app.
type User struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	PictureURL  string `json:"pictureUrl"`
}

// Group is a set of people splitting bills together.
type Group struct {
	ID          string    `json:"id"`
	LineGroupID string    `json:"lineGroupId,omitempty"`
	Name        string    `json:"name"`
	CreatedBy   string    `json:"createdBy"`
	CreatedAt   time.Time `json:"createdAt"`
	Members     []User    `json:"members,omitempty"`
}

// Share is one person's portion of a bill.
type Share struct {
	UserID string       `json:"userId"`
	Amount money.Satang `json:"amount"`
}

// Bill is one expense paid by one person on behalf of several.
type Bill struct {
	ID        string       `json:"id"`
	GroupID   string       `json:"groupId"`
	PayerID   string       `json:"payerId"`
	Title     string       `json:"title"`
	Total     money.Satang `json:"total"`
	Note      string       `json:"note,omitempty"`
	CreatedBy string       `json:"createdBy"`
	CreatedAt time.Time    `json:"createdAt"`
	Shares    []Share      `json:"shares,omitempty"`
}

// Settlement is a payment one member made to another to clear their balance.
type Settlement struct {
	ID        string       `json:"id"`
	GroupID   string       `json:"groupId"`
	FromUser  string       `json:"fromUser"`
	ToUser    string       `json:"toUser"`
	Amount    money.Satang `json:"amount"`
	Note      string       `json:"note,omitempty"`
	CreatedAt time.Time    `json:"createdAt"`
}

// Ledger is a group's running totals per member.
type Ledger struct {
	UserID string       `json:"userId"`
	Paid   money.Satang `json:"paid"`
	Owed   money.Satang `json:"owed"`
}
