package advanced

import (
	"testing"

	"github.com/warmbly/warmbly/internal/models"
)

// The IMAP sync writes an address as "Name (addr)" (imap.GetAddressName), where
// RFC 5322 puts it in angle brackets. mail.ParseAddress reads the parentheses
// as a comment and rejects the field, so every reply synced over IMAP failed
// the inbound guard added for stop-on-reply and was dropped without a word: on
// one instance 466 of 552 stored messages carry the parenthesised form.
func TestParseAddressField(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"parenthesised, as the IMAP sync writes it", "João Andrade (joao.andrade@rakhiapp.com.br)", "joao.andrade@rakhiapp.com.br"},
		{"angle brackets, RFC 5322", "João Andrade <joao.andrade@rakhiapp.com.br>", "joao.andrade@rakhiapp.com.br"},
		{"bare address", "joao.andrade@rakhiapp.com.br", "joao.andrade@rakhiapp.com.br"},
		{"parenthesised with no display name", "(billing@example.com)", "billing@example.com"},
		{"uppercase is folded", "Sales (Sales@Example.COM)", "sales@example.com"},
		{"a real comment is not an address", "someone@example.com (out of office)", "someone@example.com"},
		{"empty", "   ", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseAddressField(tc.raw); got != tc.want {
				t.Errorf("parseAddressField(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestMessageAddressesMailboxAcceptsParenthesisedTo(t *testing.T) {
	account := &models.Email{Email: "joao.andrade@rakhiapp.com.br"}
	tests := []struct {
		name string
		to   []string
		want bool
	}{
		{"parenthesised To reaches the mailbox", []string{"João Andrade (joao.andrade@rakhiapp.com.br)"}, true},
		{"angle-bracket To reaches the mailbox", []string{"João Andrade <joao.andrade@rakhiapp.com.br>"}, true},
		{"someone else's mailbox does not", []string{"Outra Pessoa (outra@example.com)"}, false},
		{"no recipients at all", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := &models.EmailMessageStoreData{ToAddr: tc.to}
			if got := messageAddressesMailbox(msg, account); got != tc.want {
				t.Errorf("messageAddressesMailbox = %v, want %v", got, tc.want)
			}
		})
	}
}
