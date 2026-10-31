#!/usr/bin/env python3
"""Verify the portable ISUReal seed without a database or reference source."""
import argparse
import collections
import gzip
import hashlib
import json
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2] / 'data' / 'initial'
EXPECTED_COUNTS = {
    'accounts': 5000, 'administrators': 1, 'posting-windows': 1095,
    'friend-requests': 28970, 'friendships': 24985, 'follows': 10020,
    'blocks': 250, 'posts': 625111, 'realmojis': 1199348,
    'comments': 546011, 'notifications': 1825000, 'images': 1826950,
}
EXPECTED_IMAGE_BYTES = 13713322751

def records(name, root=ROOT):
    for path in sorted(root.glob(name + '*.jsonl.gz')):
        with gzip.open(path, 'rt', encoding='utf-8') as stream:
            for line in stream:
                yield json.loads(line)

def file_record(path):
    digest = hashlib.sha256()
    with path.open('rb') as stream:
        for block in iter(lambda: stream.read(8 * 1024 * 1024), b''):
            digest.update(block)
    return {'bytes': path.stat().st_size, 'sha256': digest.hexdigest()}

def require(condition, message):
    if not condition:
        raise ValueError(message)

def validate():
    counts, files = {}, {}
    def rows(name):
        count, seen = 0, set()
        for row in records(name):
            require(row['id'] not in seen, f'{name}: duplicate id')
            seen.add(row['id'])
            count += 1
            yield row
        counts[name] = count
    accounts = {r['id']: r for r in rows('accounts')}
    require(len({a['username'] for a in accounts.values()}) == len(accounts), 'duplicate username')
    require(all(len(a['secretKeySha256']) == 64 for a in accounts.values()), 'invalid account key')
    admins = list(rows('administrators'))
    require(any(a['username'] == 'admin' and len(a['passwordSha256']) == 64 for a in admins), 'administrator mismatch')
    windows = {r['id']: r for r in rows('posting-windows')}
    require(len({(r['region'], r['localDate']) for r in windows.values()}) == len(windows), 'duplicate window date')
    require(not any(r['endedAt'] is None for r in windows.values()), 'initial active window')
    requests = {r['id']: r for r in rows('friend-requests')}
    for r in requests.values():
        require(r['requesterId'] in accounts and r['recipientId'] in accounts, 'request user missing')
    relationship_states = collections.Counter(r['status'] for r in requests.values())
    pairs = set()
    for r in rows('friendships'):
        require(r['user1Id'] in accounts and r['user2Id'] in accounts, 'friend user missing')
        pair = tuple(sorted((r['user1Id'], r['user2Id'])))
        require(pair not in pairs and pair[0] != pair[1], 'duplicate/self friendship')
        pairs.add(pair)
        if r['requestId'] is not None:
            req = requests.get(r['requestId'])
            require(req is not None and set(pair) == {req['requesterId'], req['recipientId']} and req['status'] == 'accepted', 'friend request mismatch')
    for name, first, second in [('follows', 'followerId', 'followeeId'), ('blocks', 'blockerId', 'blockedId')]:
        seen = set()
        for r in rows(name):
            pair = (r[first], r[second])
            require(all(u in accounts for u in pair) and pair[0] != pair[1] and pair not in seen, name + ': invalid pair')
            seen.add(pair)
    expected_images = {}
    for a in accounts.values():
        if a['profileImageId'] is not None:
            expected_images[a['profileImageId']] = a['id']
    post_authors, day_counts = {}, collections.defaultdict(lambda: [0, 0])
    deleted_posts = 0
    for p in rows('posts'):
        require(p['authorId'] in accounts and p['postingWindowId'] in windows, 'post reference missing')
        post_authors[p['id']] = p['authorId']
        expected_images[p['imageId']] = p['authorId']
        deleted = p['deletedAt'] is not None
        deleted_posts += deleted
        day_counts[(p['authorId'], windows[p['postingWindowId']]['localDate'])][int(deleted)] += 1
    reaction_states = collections.Counter()
    current_reactions = set()
    for r in rows('realmojis'):
        require(r['postId'] in post_authors and r['reactorId'] in accounts, 'reaction reference missing')
        expected_images[r['imageId']] = r['reactorId']
        state = 'deleted' if r['deletedAt'] is not None else 'replaced' if r['replacedAt'] is not None else 'current'
        reaction_states[state] += 1
        if state == 'current':
            pair = (r['postId'], r['reactorId'])
            require(pair not in current_reactions, 'duplicate current reaction')
            current_reactions.add(pair)
    del current_reactions
    deleted_comments = 0
    for r in rows('comments'):
        require(r['postId'] in post_authors and r['authorId'] in accounts, 'comment reference missing')
        deleted_comments += r['deletedAt'] is not None
    notification_pairs = set()
    for r in rows('notifications'):
        require(r['userId'] in accounts and r['postingWindowId'] in windows, 'notification reference missing')
        require(r['createdAt'] == windows[r['postingWindowId']]['startsAt'], 'notification start mismatch')
        pair = (r['userId'], r['postingWindowId'])
        require(pair not in notification_pairs, 'duplicate notification')
        notification_pairs.add(pair)
    del notification_pairs
    image_root = ROOT / 'images'
    image_manifest = json.loads((image_root / 'manifest.json').read_text())
    image_ids, image_count, image_bytes = set(), 0, 0
    stream, part, offset, part_digest = None, None, 0, None
    expected_parts = {p['file']: p for p in image_manifest['parts']}
    require({p.name for p in image_root.glob('*.bin')} == set(expected_parts), 'image file set mismatch')
    checked_parts = {}
    def close_part():
        if stream:
            require(stream.read(1) == b'', 'trailing image bytes: ' + part)
            stream.close()
            record = {'bytes': offset, 'sha256': part_digest.hexdigest()}
            require(record == {k: expected_parts[part][k] for k in record}, 'image part hash mismatch: ' + part)
            checked_parts[part] = record
    try:
        for r in records('index.part-', image_root):
            require(r['id'] not in image_ids and r['ownerId'] in accounts, 'invalid image owner/id')
            image_ids.add(r['id'])
            if r['id'] in expected_images:
                require(expected_images.pop(r['id']) == r['ownerId'], 'image reference owner mismatch')
            if part != r['part']:
                close_part()
                part, offset = r['part'], 0
                require(part in expected_parts and part not in checked_parts, 'invalid part')
                stream = (image_root / part).open('rb')
                part_digest = hashlib.sha256()
            require(r['offset'] == offset, 'image offset mismatch')
            data = stream.read(r['size'])
            require(len(data) == r['size'] and hashlib.sha256(data).hexdigest() == r['sha256'], 'image bytes mismatch')
            part_digest.update(data)
            offset += len(data)
            image_count += 1
            image_bytes += len(data)
        close_part()
    finally:
        if stream and not stream.closed:
            stream.close()
    require(not expected_images, f'{len(expected_images)} image references missing')
    require(image_count == image_manifest['images'] and image_bytes == image_manifest['bytes'], 'image totals mismatch')
    require(set(checked_parts) == set(expected_parts), 'unindexed image parts')
    counts['images'] = image_count
    require(counts == EXPECTED_COUNTS, 'record counts differ from source counts')
    require(image_bytes == EXPECTED_IMAGE_BYTES, 'image bytes differ from source bytes')
    for path in sorted(ROOT.glob('*.jsonl.gz')):
        files[path.relative_to(ROOT).as_posix()] = file_record(path)
    for path in sorted(image_root.glob('index.part-*.jsonl.gz')) + [image_root / 'manifest.json']:
        files['images/' + path.name] = file_record(path)
    project = ROOT.parents[1]
    static_manifest = json.loads((project / 'tools/contest/static-manifest.json').read_text())
    for name, expected in static_manifest.items():
        require(file_record(project / 'public' / name) == expected, 'static asset mismatch: ' + name)
    return {
        'staticFiles': static_manifest,
        'formatVersion': 1,
        'boundary': '2026-10-28T01:36:31.009580Z',
        'counts': counts,
        'files': files,
        'imageBytes': image_bytes,
        'validation': {
            'accountStates': dict(collections.Counter(a['status'] for a in accounts.values())),
            'accountRegions': dict(collections.Counter(a['region'] for a in accounts.values())),
            'friendRequestStates': dict(relationship_states),
            'reactionStates': dict(reaction_states),
            'deletedPosts': deleted_posts,
            'deletedComments': deleted_comments,
            'maxActivePostsPerUserDate': max(v[0] for v in day_counts.values()),
            'maxDeletedPostsPerUserDate': max(v[1] for v in day_counts.values()),
            'initialCurrentWindows': 0,
            'initialUserSessions': 0,
            'initialAdminSessions': 0,
            'initialIPBans': 0,
            'checked': ['unique record IDs', 'account/window/post/relationship references',
                        'friendship request correspondence', 'notification time and uniqueness',
                        'image owner references', 'all image offsets and SHA-256',
                        'all image part SHA-256', 'all metadata file SHA-256'],
            'notChecked': ['all JPEG decoding', 'all API behavior', 'concurrent HTTP operations',
                           'new implementation restart persistence', 'new implementation benchmark']
        }
    }

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--write-manifest', action='store_true')
    args = parser.parse_args()
    result = validate()
    destination = ROOT / 'manifest.json'
    if args.write_manifest:
        destination.write_text(json.dumps(result, ensure_ascii=False, indent=2) + '\n')
    else:
        require(result == json.loads(destination.read_text()), 'seed differs from recorded manifest')
    print(json.dumps({'counts': result['counts'], 'imageBytes': result['imageBytes'], 'validation': 'passed'}, ensure_ascii=False))

if __name__ == '__main__':
    main()
