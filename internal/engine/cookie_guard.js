import crypto from 'crypto';

const originProofKey = '__ORIGIN_PROOF_KEY__';

function originProof(r) {
    const nonce = r.args.nonce || '';
    if (!/^[a-f0-9]{64}$/.test(nonce)) { r.return(400); return; }
    const proof = crypto.createHmac('sha256', originProofKey)
        .update(r.variables.host.toLowerCase() + '\n' + nonce).digest('hex');
    r.headersOut['Cache-Control'] = 'no-store, private';
    r.headersOut['Content-Type'] = 'text/plain';
    r.return(200, proof + '\n');
}

// Cookie names are case-sensitive. Preserve every application cookie while
// removing every occurrence of the gateway's reserved management credential.
const reserved = 'w103d_gateway_session';

function isReserved(pair) {
    return pair.split('=', 1)[0].trim() === reserved;
}

function requestCookie(r) {
    const kept = [];
    r.rawHeadersIn.forEach(function (header) {
        if (header[0].toLowerCase() !== 'cookie') return;
        header[1].split(';').forEach(function (pair) {
            const value = pair.trim();
            if (value && !isReserved(value)) kept.push(value);
        });
    });
    return kept.join('; ');
}

function responseCookies(r) {
    const cookies = r.headersOut['Set-Cookie'];
    if (!cookies) return;
    const values = Array.isArray(cookies) ? cookies : [cookies];
    const kept = values.filter(function (value) {
        return !isReserved(value.split(';', 1)[0]);
    });
    if (kept.length) r.headersOut['Set-Cookie'] = kept;
    else delete r.headersOut['Set-Cookie'];
}

export default { requestCookie, responseCookies, originProof };
