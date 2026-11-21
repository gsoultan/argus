package control

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
)

/*
How each person likes the console laid out, kept on the control plane so the
choice follows them to the next sign-in and to another browser.

Only the settings named in uiPreferenceChoices can be stored, each with its
fixed set of values. The table is keyed by the signed-in person's email, so a
caller cannot write a row for anyone else, and the list of keys is bounded,
so a caller cannot grow it. None of this changes what anyone may reach, which
is why a change is not written to the audit chain.
*/

// uiPreferenceChoices is every setting and the values it may take. The first
// value is the default, sent for a setting the person never chose.
var uiPreferenceChoices = map[string][]string{
	// The Assets list as cards or as a table.
	"assets.view": {"cards", "table"},
	// The sidebar open with labels, or folded to icons.
	"sidebar": {"expanded", "collapsed"},
}

// ErrUnknownUIPreference is a key or a value the console does not have.
var ErrUnknownUIPreference = errors.New("unknown preference")

func validUIPreference(key, value string) error {
	vals, ok := uiPreferenceChoices[key]
	if !ok {
		keys := make([]string, 0, len(uiPreferenceChoices))
		for k := range uiPreferenceChoices {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return errors.Join(ErrUnknownUIPreference, errors.New("the settings are "+strings.Join(keys, ", ")))
	}
	for _, v := range vals {
		if v == value {
			return nil
		}
	}
	return errors.Join(ErrUnknownUIPreference, errors.New(key+" is one of "+strings.Join(vals, ", ")))
}

// UIPreferences reads email's choices, with the default for any not made.
func (s *Store) UIPreferences(ctx context.Context, email string) (map[string]string, error) {
	out := make(map[string]string, len(uiPreferenceChoices))
	for k, vals := range uiPreferenceChoices {
		out[k] = vals[0]
	}
	rows, err := s.db(ctx).Query(ctx, `SELECT key, value FROM ui_preferences WHERE email = $1`, strings.ToLower(email))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		// A row a later version wrote, or a value since retired, is left out
		// rather than sent to a console that would not know it.
		if validUIPreference(k, v) == nil {
			out[k] = v
		}
	}
	return out, rows.Err()
}

// SetUIPreference stores one of email's choices.
func (s *Store) SetUIPreference(ctx context.Context, email, key, value string) error {
	if err := validUIPreference(key, value); err != nil {
		return err
	}
	_, err := s.db(ctx).Exec(ctx, `
		INSERT INTO ui_preferences (email, key, value) VALUES ($1, $2, $3)
		ON CONFLICT (email, key) DO UPDATE SET value = $3, updated_at = now()`,
		strings.ToLower(email), key, value)
	return err
}

func (a *API) getUIPreferences(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	prefs, err := a.store.UIPreferences(r.Context(), sess.Email)
	if err != nil {
		a.fail(w, "preferences", err)
		return
	}
	writeJSON(w, http.StatusOK, prefs)
}

// putUIPreference takes {"key": "...", "value": "..."} and answers with every
// choice, as GET does.
func (a *API) putUIPreference(w http.ResponseWriter, r *http.Request) {
	sess, ok := a.authenticate(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var in struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.store.SetUIPreference(r.Context(), sess.Email, in.Key, in.Value); err != nil {
		if errors.Is(err, ErrUnknownUIPreference) {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		a.fail(w, "save preference", err)
		return
	}
	prefs, err := a.store.UIPreferences(r.Context(), sess.Email)
	if err != nil {
		a.fail(w, "preferences", err)
		return
	}
	writeJSON(w, http.StatusOK, prefs)
}
