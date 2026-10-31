"""Observe cross-region API behavior and durable state through the public router."""
import argparse
import base64
import datetime
import hashlib
import io
import json
import os
import time
from pathlib import Path
import urllib.error
import urllib.request
import zipfile

BASE = 'http://13.192.72.111:8080'
COUNTRY = {'asia': 'JP', 'europe': 'GB', 'america': 'US'}
STATE = Path('generated/validation/restart-state.json')
PROFILE_JPEG = base64.b64decode(
    '/9j/2wCEAAgGBgcGBQgHBwcJCQgKDBQNDAsLDBkSEw8UHRofHh0aHBwgJC4nICIsIxwcKDcpLDAxNDQ0Hyc5PTgyPC4zNDIBCQkJDAsMGA0NGDIhHCEyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMjIyMv/AABEIAAIAAgMBIgACEQEDEQH/xAGiAAABBQEBAQEBAQAAAAAAAAAAAQIDBAUGBwgJCgsQAAIBAwMCBAMFBQQEAAABfQECAwAEEQUSITFBBhNRYQcicRQygZGhCCNCscEVUtHwJDNicoIJChYXGBkaJSYnKCkqNDU2Nzg5OkNERUZHSElKU1RVVldYWVpjZGVmZ2hpanN0dXZ3eHl6g4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2drh4uPk5ebn6Onq8fLz9PX29/j5+gEAAwEBAQEBAQEBAQAAAAAAAAECAwQFBgcICQoLEQACAQIEBAMEBwUEBAABAncAAQIDEQQFITEGEkFRB2FxEyIygQgUQpGhscEJIzNS8BVictEKFiQ04SXxFxgZGiYnKCkqNTY3ODk6Q0RFRkdISUpTVFVWV1hZWmNkZWZnaGlqc3R1dnd4eXqCg4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2dri4+Tl5ufo6ery8/T19vf4+fr/2gAMAwEAAhEDEQA/APn+iiigD//Z'
) + b'preserved original trailer'


def api(method, path, token='', body=None, region='asia', expected=200, raw_body=None, content_type='image/jpeg'):
    headers = {'X-Country-Code': COUNTRY[region]}
    if token:
        headers['Authorization'] = 'Bearer ' + token
    if body is not None:
        headers['Content-Type'] = 'application/json'
        body = json.dumps(body).encode()
    if raw_body is not None:
        headers['Content-Type'] = content_type
        body = raw_body
    request = urllib.request.Request(BASE + path, data=body, headers=headers, method=method)
    for attempt in range(3 if method == 'GET' else 1):
        try:
            response = urllib.request.urlopen(request, timeout=10 if method == 'GET' else 30)
            payload = response.read()
            break
        except urllib.error.HTTPError as e:
            response = e
            payload = response.read()
            break
        except (urllib.error.URLError, TimeoutError):
            if method != 'GET' or attempt == 2:
                raise
            print('retrying a public read after a connection timeout', flush=True)
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


def wait_image_revoked(path, region):
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        request = urllib.request.Request(BASE + path, headers={'X-Country-Code': COUNTRY[region]})
        try:
            with urllib.request.urlopen(request, timeout=2) as response:
                response.read()
        except urllib.error.HTTPError as e:
            if e.code == 403:
                assert time.monotonic() <= deadline, 'image invalidation exceeded five seconds'
                return
            raise
        time.sleep(0.02)
    raise AssertionError('image remained available five seconds after update success')


def capture():
    initial_history()
    admin = api('POST', '/api/admin/auth/login', body={'username': os.environ['ISUREAL_ADMIN_USERNAME'], 'password': os.environ['ISUREAL_ADMIN_PASSWORD']})['token']
    saved = []
    for region in COUNTRY:
        name = 'bootstrap_' + region + '_' + os.urandom(5).hex()
        registration = api('POST', '/api/auth/register', body={'username': name, 'displayName': name, 'region': region}, region=region, expected=201)
        login = api('POST', '/api/auth/login', body={'username': name, 'secretKey': registration['secretKey']}, region=region)
        token = login['token']
        api('PATCH', '/api/me', token, {'bio': 'durable-' + region}, region)
        api('PUT', '/api/me/profile-image', token, region=region, expected=204, raw_body=PROFILE_JPEG)
        me = api('GET', '/api/me', token, region=region)
        assert me['bio'] == 'durable-' + region
        for _ in range(2):
            assert api('GET', me['profileImageUrl'], region=region) == PROFILE_JPEG
        old_url = me['profileImageUrl']
        api('PUT', '/api/me/profile-image', token, region=region, expected=204, raw_body=PROFILE_JPEG)
        wait_image_revoked(old_url, region)
        me = api('GET', '/api/me', token, region=region)
        old_url = me['profileImageUrl']
        api('DELETE', '/api/me/profile-image', token, region=region, expected=204)
        wait_image_revoked(old_url, region)
        api('PUT', '/api/me/profile-image', token, region=region, expected=204, raw_body=PROFILE_JPEG)
        me = api('GET', '/api/me', token, region=region)
        for other in COUNTRY:
            if other != region:
                api('GET', '/api/me', token, region=other, expected=403)
        for image_region in COUNTRY:
            assert api('GET', me['profileImageUrl'], region=image_region) == PROFILE_JPEG
        current = api('GET', '/api/posting-window/current', token, region=region)['window']
        next_date = datetime.date(2030, 1, 1)
        if current:
            next_date = max(next_date, datetime.date.fromisoformat(current['localDate']) + datetime.timedelta(days=1))
        api('POST', '/api/admin/posting-window', admin, {'region': region, 'localDate': next_date.isoformat()}, expected=201)
        boundary = 'isureal-public-restart-post'
        multipart = b''
        for key, value in [('caption', 'durable-' + region), ('visibility', 'public'), ('location_kind', 'none')]:
            multipart += f'--{boundary}\r\nContent-Disposition: form-data; name="{key}"\r\n\r\n{value}\r\n'.encode()
        multipart += f'--{boundary}\r\nContent-Disposition: form-data; name="image"; filename="post.jpg"\r\nContent-Type: image/jpeg\r\n\r\n'.encode() + PROFILE_JPEG + f'\r\n--{boundary}--\r\n'.encode()
        post = api('POST', '/api/posts', token, region=region, expected=201, raw_body=multipart, content_type='multipart/form-data; boundary=' + boundary)
        assert api('GET', '/api/posts/me/current', token, region=region)['id'] == post['id']
        saved.append({'region': region, 'token': token, 'id': me['id'], 'bio': me['bio'], 'profileImageUrl': me['profileImageUrl'], 'postId': post['id'], 'postImageUrl': post['imageUrl']})
    for entry, other in zip(saved, saved[1:] + saved[:1]):
        assert api('GET', '/api/posts/' + other['postId'], entry['token'], region=entry['region'])['id'] == other['postId']
    # A relationship written through Europe must be visible through America.
    request = api('POST', '/api/friend-requests', saved[1]['token'], {'recipientId': saved[2]['id']}, 'europe', 201)
    api('POST', '/api/friend-requests/' + request['relationId'] + '/accept', saved[2]['token'], region='america')
    for entry, other in [(saved[1], saved[2]), (saved[2], saved[1])]:
        friends = api('GET', '/api/relationships/friends', entry['token'], region=entry['region'])
        assert any(v['user']['id'] == other['id'] for v in friends['items'])
    STATE.parent.mkdir(exist_ok=True, parents=True)
    STATE.write_text(json.dumps(saved))
    STATE.chmod(0o600)
    print('cross-region authentication, original images, replacement/deletion expiry, and friendship passed; restart state captured')


def verify():
    saved = json.loads(STATE.read_text())
    for entry in saved:
        me = api('GET', '/api/me', entry['token'], region=entry['region'])
        assert me['id'] == entry['id'] and me['bio'] == entry['bio']
        for image_region in COUNTRY:
            assert api('GET', entry['profileImageUrl'], region=image_region) == PROFILE_JPEG
            assert api('GET', entry['postImageUrl'], region=image_region) == PROFILE_JPEG
        assert api('GET', '/api/posts/me/current', entry['token'], region=entry['region'])['id'] == entry['postId']
    for entry, other in [(saved[1], saved[2]), (saved[2], saved[1])]:
        friends = api('GET', '/api/relationships/friends', entry['token'], region=entry['region'])
        assert any(v['user']['id'] == other['id'] for v in friends['items'])
    print('public read-after-restart: account, token, original profile/post images, posts, and cross-region friendship retained')


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('mode', choices=['history', 'capture', 'verify'])
    mode = parser.parse_args().mode
    {'history': initial_history, 'capture': capture, 'verify': verify}[mode]()
