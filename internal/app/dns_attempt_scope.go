package app

import "sort"

func dnsAttemptKey(host, account, zone string) string { return account + "\x00" + zone + "\x00" + host }
func selectDNSAttempt(j *dnsJournal, host, account, zone string) {
	key := dnsAttemptKey(host, account, zone)
	if j.Attempt != nil && dnsAttemptKey(j.Attempt.Host, j.Attempt.AccountID, j.Attempt.Zone) == key {
		return
	}
	if j.Attempts == nil {
		j.Attempts = map[string]*dnsAttempt{}
	}
	if j.Attempt != nil {
		v := j.Attempt
		j.Attempts[dnsAttemptKey(v.Host, v.AccountID, v.Zone)] = v
	}
	j.Attempt = j.Attempts[key]
	delete(j.Attempts, key)
}
func promoteDNSAttempt(j *dnsJournal) {
	if j.Attempt != nil || len(j.Attempts) == 0 {
		return
	}
	keys := make([]string, 0, len(j.Attempts))
	for key := range j.Attempts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	j.Attempt = j.Attempts[keys[0]]
	delete(j.Attempts, keys[0])
}
func dnsUnknownForHost(j dnsJournal, host string) bool {
	if j.Attempt != nil && j.Attempt.Host == host {
		return true
	}
	for _, v := range j.Attempts {
		if v != nil && v.Host == host {
			return true
		}
	}
	return false
}

func dnsAttemptForHost(j dnsJournal, host string) *dnsAttempt {
	if j.Attempt != nil && j.Attempt.Host == host {
		return j.Attempt
	}
	for _, v := range j.Attempts {
		if v != nil && v.Host == host {
			return v
		}
	}
	return nil
}
func dnsUnknownUsesAccount(j dnsJournal, id string) bool {
	if j.Attempt != nil && j.Attempt.AccountID == id {
		return true
	}
	for _, v := range j.Attempts {
		if v != nil && v.AccountID == id {
			return true
		}
	}
	return false
}
func selectTXTAttempt(j *dnsTXTJournal, host, account, zone string) {
	key := dnsAttemptKey(host, account, zone)
	if j.Attempt != nil && dnsAttemptKey(j.Attempt.Host, j.Attempt.AccountID, j.Attempt.Zone) == key {
		return
	}
	if j.Attempts == nil {
		j.Attempts = map[string]*dnsTXTAttempt{}
	}
	if j.Attempt != nil {
		v := j.Attempt
		j.Attempts[dnsAttemptKey(v.Host, v.AccountID, v.Zone)] = v
	}
	j.Attempt = j.Attempts[key]
	delete(j.Attempts, key)
}
func promoteTXTAttempt(j *dnsTXTJournal) {
	if j.Attempt != nil || len(j.Attempts) == 0 {
		return
	}
	keys := make([]string, 0, len(j.Attempts))
	for key := range j.Attempts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	j.Attempt = j.Attempts[keys[0]]
	delete(j.Attempts, keys[0])
}
