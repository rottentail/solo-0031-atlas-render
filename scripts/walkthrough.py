#!/usr/bin/env python3
"""Run bounded production flows against a temporary Atlas Render server."""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import socket
import struct
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import zlib

ROOT = Path(__file__).resolve().parents[1]


def sample():
    return {"width": 320, "height": 240, "background": "#F5F1E8", "layers": [
        {"id": "plate", "kind": "rectangle", "width": 220, "height": 150,
         "transform": {"x": 45, "y": 35, "scaleX": 1, "scaleY": 1, "degrees": 5},
         "opacity": 1, "fill": {"kind": "linear", "color": "#145A70", "endColor": "#32A5A0", "from": {"x": 0, "y": 0}, "to": {"x": 220, "y": 150}}},
        {"id": "accent", "kind": "ellipse", "width": 130, "height": 130,
         "transform": {"x": 150, "y": 95, "scaleX": 1, "scaleY": 1, "degrees": 0},
         "opacity": .85, "fill": {"kind": "solid", "color": "#E87E4F"}},
        {"id": "inset", "kind": "group", "transform": {"x": 150, "y": 100, "scaleX": 1, "scaleY": 1, "degrees": -8}, "opacity": .9,
         "clip": {"x": 0, "y": 0, "width": 90, "height": 75}, "children": [
             {"id": "facet", "kind": "polygon", "width": 100, "height": 100,
              "transform": {"scaleX": 1, "scaleY": 1}, "opacity": .8,
              "fill": {"kind": "solid", "color": "#FFF4D5"}, "points": [{"x": 10, "y": 90}, {"x": 50, "y": 0}, {"x": 100, "y": 90}]}]}]}


def tiny_png():
    def chunk(tag, data):
        return struct.pack('>I', len(data)) + tag + data + struct.pack('>I', zlib.crc32(tag + data))
    return b'\x89PNG\r\n\x1a\n' + chunk(b'IHDR', struct.pack('>IIBBBBB', 2, 2, 8, 6, 0, 0, 0)) + chunk(b'IDAT', zlib.compress(b'\0' + bytes([240, 40, 80, 255] * 2) + b'\0' + bytes([40, 80, 240, 128] * 2))) + chunk(b'IEND', b'')


class Demo:
    def __init__(self, binary, directory):
        self.binary = binary
        self.directory = directory
        self.proc = None
        self.log = open(directory / 'server.log', 'ab')
        self.start()

    def start(self):
        with socket.socket() as sock:
            sock.bind(('127.0.0.1', 0))
            port = sock.getsockname()[1]
        self.url = f'http://127.0.0.1:{port}'
        env = dict(os.environ, ATLAS_ADDR=f'127.0.0.1:{port}', ATLAS_STATE=str(self.directory / 'state.json'))
        self.proc = subprocess.Popen([str(self.binary)], env=env, stdout=self.log, stderr=self.log)
        for _ in range(100):
            try:
                self.call('GET', '/healthz')
                return
            except (urllib.error.URLError, ConnectionError):
                if self.proc.poll() is not None:
                    raise RuntimeError((self.directory / 'server.log').read_text())
                time.sleep(.03)
        raise RuntimeError('server startup deadline exceeded')

    def stop(self, abrupt=False):
        if self.proc and self.proc.poll() is None:
            (self.proc.kill if abrupt else self.proc.terminate)()
            self.proc.wait(timeout=15)

    def call(self, method, path, body=None, status=200, headers=None):
        hdr = dict(headers or {})
        if isinstance(body, (dict, list)):
            body = json.dumps(body).encode()
            hdr['Content-Type'] = 'application/json'
        request = urllib.request.Request(self.url + path, data=body, method=method, headers=hdr)
        try:
            response = urllib.request.urlopen(request, timeout=15)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            data = response.read()
            if response.status != status:
                raise RuntimeError(f'{method} {path}: expected {status}, received {response.status}: {data[:500]!r}')
            if response.headers.get('Content-Type', '').startswith('application/json'):
                return json.loads(data)
            return data

    def wait(self, job_id, wanted=('succeeded',)):
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            job = self.call('GET', '/v1/jobs/' + job_id)
            if job['state'] in wanted:
                return job
            if job['state'] in ('failed', 'cancelled'):
                raise RuntimeError(f'unexpected job outcome: {job}')
            time.sleep(.025)
        raise RuntimeError('render deadline exceeded')

    def submit(self, snapshot, key='render-one', scale=1):
        return self.call('POST', '/v1/jobs', {'snapshotId': snapshot['id'], 'scale': scale}, 202, {'Idempotency-Key': key})

    def submit_batch(self, snapshot, scales, key):
        return self.call('POST', '/v1/batches', {'snapshotId': snapshot['id'], 'scales': scales}, 202, {'Idempotency-Key': key})

    def get_batch(self, batch_id):
        return self.call('GET', '/v1/batches/' + batch_id)

    def wait_batch(self, batch_id, wanted=('settled', 'completed'), timeout=20):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            batch = self.get_batch(batch_id)
            if batch['state'] in wanted:
                return batch
            time.sleep(.025)
        raise RuntimeError('batch deadline exceeded')

    def wait_job_state(self, job_id, wanted):
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            job = self.call('GET', '/v1/jobs/' + job_id)
            if job['state'] in wanted:
                return job
            time.sleep(.025)
        raise RuntimeError(f'job {job_id} never reached {wanted}')

    def wait_pending_below(self, limit):
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            jobs = self.call('GET', '/v1/status')['jobs']
            if jobs.get('queued', 0) + jobs.get('running', 0) < limit:
                return
            time.sleep(.1)
        raise RuntimeError('queue never drained below the limit')


def heavy_scene(width, height, layers):
    doc = sample()
    doc['width'] = width
    doc['height'] = height
    doc['layers'] = [dict(doc['layers'][0], id=f'sheet-{i}', width=width, height=height,
                          transform={'scaleX': 1, 'scaleY': 1}) for i in range(layers)]
    return doc


def member_map(batch):
    return {m['scale']: m for m in batch['members']}


def run_batch(demo, workflow, snapshot, current, put):
    if workflow == 'batch-render':
        # 1. Whole-set commitment: both members exist in one response.
        batch = demo.submit_batch(snapshot, [2, 1], 'batch-happy')
        assert batch['state'] == 'active'
        assert batch['scales'] == [1, 2]
        job_ids = [m['jobId'] for m in batch['members']]
        assert len(set(job_ids)) == 2
        # 2. Idempotent replay returns the identical set; a changed request
        #    under the same key conflicts and must not create more jobs.
        replay = demo.submit_batch(snapshot, [1, 2], 'batch-happy')
        assert replay['id'] == batch['id'] and [m['jobId'] for m in replay['members']] == job_ids
        demo.call('POST', '/v1/batches', {'snapshotId': snapshot['id'], 'scales': [1]}, 409,
                  {'Idempotency-Key': 'batch-happy'})
        # Single-job receipt names never collide with batch receipts.
        demo.call('POST', '/v1/jobs', {'snapshotId': snapshot['id'], 'scale': 1}, 409,
                  {'Idempotency-Key': 'batch-happy'})
        # 3. Malformed sets are rejected before anything is committed.
        demo.call('POST', '/v1/batches', {'snapshotId': snapshot['id'], 'scales': []}, 400,
                  {'Idempotency-Key': 'batch-empty'})
        demo.call('POST', '/v1/batches', {'snapshotId': snapshot['id'], 'scales': [1, 1]}, 400,
                  {'Idempotency-Key': 'batch-dup'})
        demo.call('POST', '/v1/batches', {'snapshotId': snapshot['id'], 'scales': [3]}, 400,
                  {'Idempotency-Key': 'batch-bad-scale'})
        # 4. All members complete and the set becomes completed with real PNGs.
        done = demo.wait_batch(batch['id'])
        assert done['state'] == 'completed'
        artifacts = {}
        for member in done['members']:
            assert member['state'] == 'succeeded' and member['artifactId']
            png = demo.call('GET', '/v1/artifacts/' + member['artifactId'])
            assert png[:8] == b'\x89PNG\r\n\x1a\n'
            expected = (snapshot['document']['width'] * member['scale'],
                        snapshot['document']['height'] * member['scale'])
            assert struct.unpack('>II', png[16:24]) == expected
            artifacts[member['scale']] = member['artifactId']
        # 5. Mixed outcomes: member 1 finishes; member 2 is interrupted while
        #    it renders, and the set settles with both facts explainable.
        heavy_doc = heavy_scene(1000, 1000, 9)
        put(heavy_doc, current['revision'])
        heavy_snap = demo.call('POST', '/v1/scenes/emblem/snapshots',
                               {'expectedRevision': current['revision'] + 1}, 201)
        mixed = demo.submit_batch(heavy_snap, [1, 2], 'batch-mixed')
        first, second = mixed['members']
        demo.wait_job_state(first['jobId'], ('succeeded',))
        # Member 2 is now claimed by the worker; interrupt its in-flight render.
        demo.wait_job_state(second['jobId'], ('running',))
        demo.call('POST', '/v1/batches/' + mixed['id'] + '/cancel')
        interrupted = demo.wait_batch(mixed['id'])
        assert interrupted['state'] == 'settled'
        members = member_map(interrupted)
        assert members[1]['state'] == 'succeeded' and members[1].get('artifactId')
        assert members[2]['state'] == 'cancelled' and not members[2].get('artifactId')
        assert demo.call('GET', '/v1/jobs/' + first['jobId'])['state'] == 'succeeded'
        assert demo.call('GET', '/v1/jobs/' + second['jobId'])['state'] == 'cancelled'
        # Re-cancelling a finished set is a conflict, not a new transition.
        demo.call('POST', '/v1/batches/' + mixed['id'] + '/cancel', status=409)
        # 6. Cancel meets retry: only the cancelled member reopens; the
        #    successful member and its artifact stay untouched.
        reopened = demo.call('POST', '/v1/batches/' + mixed['id'] + '/retry', status=202)
        assert reopened['state'] == 'active'
        reopened_members = member_map(reopened)
        assert reopened_members[1]['state'] == 'succeeded' and reopened_members[1].get('artifactId')
        assert reopened_members[2]['state'] == 'queued' and reopened_members[2]['attempt'] == 2
        demo.call('POST', '/v1/batches/' + mixed['id'] + '/cancel')
        again = demo.wait_batch(mixed['id'])
        assert again['state'] == 'settled'
        assert member_map(again)[1]['state'] == 'succeeded'
        assert member_map(again)[2]['state'] == 'cancelled'
        # The final retry lets the remaining member finish; the set completes.
        demo.call('POST', '/v1/batches/' + mixed['id'] + '/retry', status=202)
        finished = demo.wait_batch(mixed['id'], wanted=('completed',))
        assert finished['state'] == 'completed'
        assert all(m['state'] == 'succeeded' for m in finished['members'])
        demo.call('POST', '/v1/batches/' + mixed['id'] + '/retry', status=409)
        # 7. Member jobs remain addressable through the historical single-job API.
        for job_id in job_ids:
            assert demo.call('GET', '/v1/jobs/' + job_id)['state'] == 'succeeded'
        status = demo.call('GET', '/v1/status')
        assert status['batchCount'] >= 2 and status['batches'].get('completed', 0) >= 2
        events = demo.call('GET', '/v1/events')
        kinds = {e['kind'] for e in events}
        assert {'batch.queued', 'batch.completed', 'batch.cancelled', 'batch.retried',
                'batch.settled', 'batch.reopened'} & kinds
        return {'workflow': workflow, 'state': done['state'],
                'artifacts': artifacts, 'mixedOutcome': 'succeeded+cancelled->completed'}

    if workflow == 'batch-recover':
        heavy_doc = heavy_scene(700, 700, 16)
        put(heavy_doc, current['revision'])
        heavy_snap = demo.call('POST', '/v1/scenes/emblem/snapshots',
                               {'expectedRevision': current['revision'] + 1}, 201)
        batch = demo.submit_batch(heavy_snap, [1, 2], 'batch-restart')
        first, second = batch['members']
        # Let the first member finish, then SIGKILL while the second runs.
        demo.wait_job_state(first['jobId'], ('succeeded',))
        demo.wait_job_state(second['jobId'], ('running', 'queued'))
        demo.stop(abrupt=True)
        demo.start()
        recovered = demo.get_batch(batch['id'])
        assert recovered['state'] == 'active'
        members = member_map(recovered)
        assert members[1]['state'] == 'succeeded'
        assert members[2]['state'] == 'queued'
        finished = demo.wait_batch(batch['id'], wanted=('completed',))
        assert all(m['state'] == 'succeeded' for m in finished['members'])
        # Restart with the set already terminal must keep it explainable.
        demo.stop(abrupt=True)
        demo.start()
        after = demo.get_batch(batch['id'])
        assert after['state'] == 'completed'
        return {'workflow': workflow, 'state': 'completed', 'jobIds': [m['jobId'] for m in finished['members']]}

    # batch-capacity: resource failures commit nothing.
    # Over-budget member: the scale-2 compile exceeds 40m layer pixels, so the
    # whole two-scale set is refused and neither member exists.
    huge = heavy_scene(1200, 1200, 8)
    put(huge, current['revision'])
    huge_snap = demo.call('POST', '/v1/scenes/emblem/snapshots',
                          {'expectedRevision': current['revision'] + 1}, 201)
    demo.call('POST', '/v1/batches', {'snapshotId': huge_snap['id'], 'scales': [1, 2]}, 429,
              {'Idempotency-Key': 'batch-over-budget'})
    status = demo.call('GET', '/v1/status')
    before_jobs = sum(status['jobs'].values())
    # Queue limit: fill the 64 slots with single jobs that outrun the short
    # submission burst but drain quickly afterwards, then a batch of two
    # cannot fit and must leave no half-set behind.
    queued_doc = heavy_scene(1100, 1100, 8)
    put(queued_doc, current['revision'] + 1)
    queued_snap = demo.call('POST', '/v1/scenes/emblem/snapshots',
                            {'expectedRevision': current['revision'] + 2}, 201)
    for i in range(64):
        demo.submit(queued_snap, f'filler-{i:02d}')
    demo.call('POST', '/v1/batches', {'snapshotId': queued_snap['id'], 'scales': [1, 2]}, 429,
              {'Idempotency-Key': 'batch-queue-full'})
    # Rejected batches create no jobs and no receipts that change outcomes.
    after_status = demo.call('GET', '/v1/status')
    assert sum(after_status['jobs'].values()) == before_jobs + 64
    pending = after_status['jobs'].get('queued', 0) + after_status['jobs'].get('running', 0)
    assert pending == 64
    # Once capacity frees up, the same semantic request under a fresh key
    # accepts the complete set atomically.
    demo.wait_pending_below(63)
    batch = demo.submit_batch(queued_snap, [1, 2], 'batch-after-drain')
    assert len(batch['members']) == 2 and batch['state'] == 'active'
    finished = demo.wait_batch(batch['id'], wanted=('completed', 'settled'), timeout=60)
    assert all(m['state'] == 'succeeded' for m in finished['members'])
    return {'workflow': workflow, 'state': 'rejected-then-accepted',
            'pendingAtLimit': pending}


def run(demo, workflow, out):
    d = sample()
    put = lambda doc, rev=0: demo.call('PUT', '/v1/scenes/emblem', {'name': '双色印记', 'expectedRevision': rev, 'document': doc})
    scene = put(d)
    if workflow == 'create-scene':
        assert demo.call('GET', '/v1/scenes/emblem')['document'] == scene['document']
        bad = sample(); bad['layers'][0]['transform']['scaleX'] = 0
        demo.call('PUT', '/v1/scenes/invalid', {'name': '无效几何', 'expectedRevision': 0, 'document': bad}, 400)
    elif workflow == 'revise-scene':
        d['background'] = '#F0F0F0'; assert put(d, 1)['revision'] == 2
        demo.call('PUT', '/v1/scenes/emblem', {'name': '旧版本', 'expectedRevision': 1, 'document': d}, 409)
    elif workflow == 'archive-scene':
        archived = demo.call('POST', '/v1/scenes/emblem/state', {'expectedRevision': 1, 'state': 'archived'})
        demo.call('POST', '/v1/scenes/emblem/snapshots', {'expectedRevision': archived['revision']}, 409)
        assert demo.call('POST', '/v1/scenes/emblem/state', {'expectedRevision': archived['revision'], 'state': 'active'})['revision'] == 3
    if workflow in ('import-asset', 'retire-asset'):
        asset = demo.call('PUT', '/v1/assets/ribbon', tiny_png(), 201)
        assert asset['width'] == 2 and asset['digest'] == hashlib.sha256(tiny_png()).hexdigest()
        demo.call('PUT', '/v1/assets/broken', b'not png', 400)
        layer = {'id': 'ribbon', 'kind': 'image', 'width': 60, 'height': 60, 'assetId': 'ribbon', 'opacity': 1, 'transform': {'x': 20, 'y': 30, 'scaleX': 1, 'scaleY': 1}}
        d['layers'].append(layer); put(d, 1)
        pinned = demo.call('POST', '/v1/scenes/emblem/snapshots', {'expectedRevision': 2}, 201)
        demo.call('POST', '/v1/assets/ribbon/retire')
        demo.call('POST', '/v1/scenes/emblem/snapshots', {'expectedRevision': 2}, 409)
        result = demo.wait(demo.submit(pinned)['id']); assert result['state'] == 'succeeded'
        return {'workflow': workflow, 'assetDigest': asset['digest'], 'pinnedRender': result['state']}
    current = demo.call('GET', '/v1/scenes/emblem')
    snapshot = demo.call('POST', '/v1/scenes/emblem/snapshots', {'expectedRevision': current['revision']}, 201)
    if workflow in ('batch-render', 'batch-recover', 'batch-capacity'):
        return run_batch(demo, workflow, snapshot, current, put)
    if workflow == 'freeze-scene':
        original_digest = snapshot['digest']; d['background'] = '#000000'; put(d, current['revision'])
        assert demo.call('GET', '/v1/snapshots/' + snapshot['id'])['digest'] == original_digest
    if workflow in ('export-package', 'import-package'):
        package = demo.call('GET', '/v1/snapshots/' + snapshot['id'] + '/package')
        restored = demo.call('POST', '/v1/imports/restored', {'name': '恢复的印记', 'package': package}, 201)
        assert restored['document'] == snapshot['document']
        corrupt = json.loads(json.dumps(package)); corrupt['snapshot']['document']['width'] += 1
        demo.call('POST', '/v1/imports/corrupt', {'name': '损坏包', 'package': corrupt}, 400)
        demo.call('GET', '/v1/scenes/corrupt', status=404)
    if workflow in ('cancel-render', 'retry-render', 'recover-render'):
        heavy = sample(); heavy['width'] = 1500; heavy['height'] = 1500
        heavy['layers'] = [dict(heavy['layers'][0], id=f'sheet-{i}', width=1500, height=1500, transform={'scaleX': 1, 'scaleY': 1}) for i in range(16)]
        put(heavy, current['revision'])
        snapshot = demo.call('POST', '/v1/scenes/emblem/snapshots', {'expectedRevision': current['revision'] + 1}, 201)
    job = demo.submit(snapshot)
    if workflow == 'submit-render':
        assert demo.submit(snapshot)['id'] == job['id']
        demo.call('POST', '/v1/jobs', {'snapshotId': snapshot['id'], 'scale': 2}, 409, {'Idempotency-Key': 'render-one'})
    if workflow == 'revoke-snapshot':
        demo.call('POST', '/v1/snapshots/' + snapshot['id'] + '/revoke')
        demo.call('POST', '/v1/jobs', {'snapshotId': snapshot['id'], 'scale': 1}, 409, {'Idempotency-Key': 'blocked-after-revoke'})
    if workflow in ('cancel-render', 'retry-render'):
        stopped = demo.call('POST', '/v1/jobs/' + job['id'] + '/cancel')
        assert stopped['state'] == 'cancelled'
        if workflow == 'cancel-render':
            time.sleep(.1); assert demo.call('GET', '/v1/jobs/' + job['id'])['state'] == 'cancelled'
            return {'workflow': workflow, 'state': 'cancelled'}
        job = demo.call('POST', '/v1/jobs/' + job['id'] + '/retry', status=202)
        assert job['attempt'] == 2
    if workflow == 'recover-render':
        demo.wait(job['id'], wanted=('running',)); demo.stop(abrupt=True); demo.start()
    result = demo.wait(job['id'])
    png = demo.call('GET', '/v1/artifacts/' + result['artifactId'])
    assert png[:8] == b'\x89PNG\r\n\x1a\n'
    assert struct.unpack('>II', png[16:24]) == (snapshot['document']['width'], snapshot['document']['height'])
    if out:
        Path(out).mkdir(parents=True, exist_ok=True); (Path(out) / 'atlas-sample.png').write_bytes(png)
    if workflow == 'compare-renders':
        other = demo.wait(demo.submit(snapshot, 'render-two')['id'])
        comparison = demo.call('POST', '/v1/compare', {'left': result['artifactId'], 'right': other['artifactId']})
        assert comparison['changedPixels'] == 0
        d['background'] = '#000000'; put(d, current['revision'])
        altered = demo.call('POST', '/v1/scenes/emblem/snapshots', {'expectedRevision': current['revision'] + 1}, 201)
        changed = demo.wait(demo.submit(altered, 'render-three')['id'])
        assert demo.call('POST', '/v1/compare', {'left': result['artifactId'], 'right': changed['artifactId']})['changedPixels'] > 0
    if workflow == 'purge-artifact':
        demo.call('DELETE', '/v1/artifacts/' + result['artifactId'])
        demo.call('GET', '/v1/artifacts/' + result['artifactId'], status=410)
        assert demo.call('GET', '/v1/jobs/' + job['id'])['state'] == 'succeeded'
    if workflow == 'observe-service':
        summary = demo.call('GET', '/v1/status'); assert summary['jobs']['succeeded'] >= 1
        events = demo.call('GET', '/v1/events'); assert any(e['kind'] == 'job.succeeded' for e in events)
    return {'workflow': workflow, 'state': result['state'], 'pngBytes': len(png), 'pngDigest': hashlib.sha256(png).hexdigest()}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--workflow', default='submit-render')
    parser.add_argument('--out')
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(prefix='atlas-walkthrough-') as temp:
        directory = Path(temp); binary = directory / 'atlas'
        subprocess.run(['go', 'build', '-o', str(binary), './cmd/atlas'], cwd=ROOT, check=True, timeout=60)
        demo = Demo(binary, directory)
        try:
            print(json.dumps(run(demo, args.workflow, args.out), ensure_ascii=False))
        finally:
            demo.stop(); demo.log.close()


if __name__ == '__main__':
    main()
