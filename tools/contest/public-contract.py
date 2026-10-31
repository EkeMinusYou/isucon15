"""Observe cross-region API behavior and durable state through the public router."""
import argparse
import hashlib
import io
import json
import os
from pathlib import Path
import urllib.error
import urllib.request
import zipfile

BASE = 'http://13.192.72.111:8080'
COUNTRY = {'asia': 'JP', 'europe': 'GB', 'america': 'US'}
STATE = Path('generated/validation/restart-state.json')


def api(method, path, token='', body=None, region='asia', expected=200):
    headers = {'X-Country-Code': COUNTRY[region]}
    if token:
        headers['Authorization'] = 'Bearer ' + token
    if body is not None:
        headers['Content-Type'] = 'application/json'
        body = json.dumps(body).encode()
    request = urllib.request.Request(BASE + path, data=body, headers=headers, method=method)
    try:
        response = urllib.request.urlopen(request, timeout=120)
    except urllib.error.HTTPError as e:
        response = e
    payload = response.read()
    assert response.status == expected, (method, path.split('?')[0], response.status, expected)
    if 'json' in response.headers.get('Content-Type', ''):
        return json.loads(payload)
    return payload


def initial_history():
    user = os.environ['ISUREAL_SMOKE_USERNAME']
    login = api('POST', '/api/auth/login', body={'username': user, 'secretKey': os.environ['ISUREAL_SMOKE_SECRET']})
    token = login['token']
    me = api('GET', '/api/me', token)
    assert me['id'] == login['user']['id']
    api('GET', '/api/me', token, region='europe', expected=403)
    memories = api('GET', '/api/memories?month=2026-10', token)
    assert memories['items'], 'initial memories missing'
    archive = zipfile.ZipFile(io.BytesIO(api('GET', '/api/me/export', token)))
    posts = json.loads(archive.read('posts.json'))
    assert posts, 'initial export missing'
    post = posts[0]
    assert archive.read(post['imagePath']) == api('GET', post['imageUrl'])
    print(f'initial history: {len(memories["items"])} monthly posts; {len(posts)} exported posts; original image bytes match')


def capture():
    initial_history()
    saved = []
    for region in COUNTRY:
        name = 'bootstrap_' + region + '_' + os.urandom(5).hex()
        registration = api('POST', '/api/auth/register', body={'username': name, 'displayName': name, 'region': region}, region=region, expected=201)
        login = api('POST', '/api/auth/login', body={'username': name, 'secretKey': registration['secretKey']}, region=region)
        token = login['token']
        api('PATCH', '/api/me', token, {'bio': 'durable-' + region}, region)
        me = api('GET', '/api/me', token, region=region)
        assert me['bio'] == 'durable-' + region
        for other in COUNTRY:
            if other != region:
                api('GET', '/api/me', token, region=other, expected=403)
        saved.append({'region': region, 'token': token, 'id': me['id'], 'bio': me['bio']})
    # A relationship written through Europe must be visible through America.
    request = api('POST', '/api/friend-requests', saved[1]['token'], {'recipientId': saved[2]['id']}, 'europe', 201)
    api('POST', '/api/friend-requests/' + request['relationId'] + '/accept', saved[2]['token'], region='america')
    for entry, other in [(saved[1], saved[2]), (saved[2], saved[1])]:
        friends = api('GET', '/api/relationships/friends', entry['token'], region=entry['region'])
        assert any(v['user']['id'] == other['id'] for v in friends['items'])
    STATE.parent.mkdir(exist_ok=True, parents=True)
    STATE.write_text(json.dumps(saved))
    STATE.chmod(0o600)
    print('cross-region authentication, profile visibility, and reciprocal friendship passed; restart state captured')


def verify():
    saved = json.loads(STATE.read_text())
    for entry in saved:
        me = api('GET', '/api/me', entry['token'], region=entry['region'])
        assert me['id'] == entry['id'] and me['bio'] == entry['bio']
    for entry, other in [(saved[1], saved[2]), (saved[2], saved[1])]:
        friends = api('GET', '/api/relationships/friends', entry['token'], region=entry['region'])
        assert any(v['user']['id'] == other['id'] for v in friends['items'])
    print('public read-after-restart: account, token, profile, and cross-region friendship retained')


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('mode', choices=['history', 'capture', 'verify'])
    mode = parser.parse_args().mode
    {'history': initial_history, 'capture': capture, 'verify': verify}[mode]()
