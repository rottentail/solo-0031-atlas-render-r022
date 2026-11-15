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
from concurrent.futures import ThreadPoolExecutor

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

    def raw(self, method, path, body=None, headers=None):
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
            if response.headers.get('Content-Type', '').startswith('application/json'):
                data = json.loads(data)
            return response.status, data

    def submit(self, snapshot, key='render-one', scale=1):
        return self.call('POST', '/v1/jobs', {'snapshotId': snapshot['id'], 'scale': scale}, 202, {'Idempotency-Key': key})


def merge_scene_edits(demo, d, put):
    """Edit individual layers, surface real conflicts, and freeze one revision."""
    def layer(doc, ident):
        stack = list(doc['layers'])
        while stack:
            candidate = stack.pop()
            if candidate['id'] == ident:
                return candidate
            stack.extend(candidate.get('children', []))
        raise AssertionError(ident)

    def patch(edit, status=200):
        return demo.call('PATCH', '/v1/scenes/emblem', edit, status)

    def order(doc, parent=''):
        layers = doc['layers'] if not parent else layer(doc, parent)['children']
        return [l['id'] for l in layers]

    def snapshots():
        return demo.call('GET', '/v1/status')['snapshotCount']

    base = demo.call('GET', '/v1/scenes/emblem')['revision']
    assert base == 1

    # Two editors read revision 1. Their layer-scoped edits merge because they
    # touch different layers.
    accent = dict(layer(d, 'accent'), fill={'kind': 'solid', 'color': '#223344'})
    first = patch({'expectedRevision': base, 'layers': [{'layer': accent}]})
    assert first['scene']['revision'] == 2 and first['merged'] is False
    plate = dict(layer(d, 'plate'))
    plate['transform'] = {'x': 45, 'y': 35, 'scaleX': 1, 'scaleY': 1, 'degrees': 20}
    second = patch({'expectedRevision': base, 'layers': [{'layer': plate}]})
    assert second['scene']['revision'] == 3 and second['merged'] is True
    current = demo.call('GET', '/v1/scenes/emblem')
    assert layer(current['document'], 'accent')['fill']['color'] == '#223344', 'unrelated edit was lost'
    assert layer(current['document'], 'plate')['transform']['degrees'] == 20

    # Same-layer changes are never silently overwritten.
    status, failed = demo.raw('PATCH', '/v1/scenes/emblem', {'expectedRevision': base, 'deleteLayers': ['accent']})
    assert status == 409 and failed['details']['currentRevision'] == 3
    assert failed['details']['conflicts'][0]['layer'] == 'accent'
    assert failed['details']['conflicts'][0]['reason'] == 'layer-modified'
    retargeted = dict(plate, transform={'x': 45, 'y': 35, 'scaleX': 1, 'scaleY': 1, 'degrees': 45})
    status, failed = demo.raw('PATCH', '/v1/scenes/emblem', {'expectedRevision': 2, 'layers': [{'layer': retargeted}]})
    assert status == 409 and failed['details']['conflicts'][0]['field'] == 'transform'
    assert demo.call('GET', '/v1/scenes/emblem')['revision'] == 3
    assert layer(demo.call('GET', '/v1/scenes/emblem')['document'], 'plate')['transform']['degrees'] == 20

    # Reordering siblings from a stale base is a conflict; applying it once
    # from the current base succeeds and keeps the other editors' layering.
    swapped = patch({'expectedRevision': 3, 'order': [{'layerIds': ['accent', 'plate', 'inset']}]})
    assert swapped['scene']['revision'] == 4
    status, failed = demo.raw('PATCH', '/v1/scenes/emblem', {'expectedRevision': 3, 'order': [{'layerIds': ['inset', 'accent', 'plate']}]})
    assert status == 409 and failed['details']['conflicts'][0]['reason'] == 'order-modified'

    # A new layer based on a stale revision is appended without disturbing the
    # committed order.
    glow = {'id': 'glow', 'kind': 'ellipse', 'width': 40, 'height': 40,
            'transform': {'x': 10, 'y': 10, 'scaleX': 1, 'scaleY': 1},
            'opacity': .5, 'fill': {'kind': 'solid', 'color': '#FFFFFF'}}
    inserted = patch({'expectedRevision': 3, 'layers': [{'layer': glow}]})
    assert inserted['scene']['revision'] == 5 and inserted['merged'] is True
    assert order(inserted['scene']['document']) == ['accent', 'plate', 'inset', 'glow']

    # Replaying the same layer edit is idempotent rather than a new revision.
    replay = patch({'expectedRevision': base, 'layers': [{'layer': plate}]})
    assert replay['scene']['revision'] == 5
    assert order(replay['scene']['document']) == ['accent', 'plate', 'inset', 'glow']

    # Group-local edits merge with concurrent edits below the group: the stale
    # inline children never overwrite the committed child layer.
    facet = dict(layer(d, 'facet'), opacity=.6)
    nested = patch({'expectedRevision': base, 'layers': [{'layer': facet}]})
    assert nested['merged'] is True
    inset = dict(layer(d, 'inset'), transform={'x': 150, 'y': 100, 'scaleX': 1, 'scaleY': 1, 'degrees': 3})
    group = patch({'expectedRevision': base, 'layers': [{'layer': inset}]})
    merged_doc = group['scene']['document']
    assert layer(merged_doc, 'facet')['opacity'] == .6, 'stale group overwrote a nested edit'
    assert layer(merged_doc, 'inset')['transform']['degrees'] == 3
    assert layer(merged_doc, 'inset')['children'][0]['id'] == 'facet'
    revision = group['scene']['revision']

    # A nested layer can be created and moved inside a group, and moving a
    # group into its own subtree is rejected outright.
    spark = {'id': 'spark', 'kind': 'rectangle', 'width': 12, 'height': 12,
             'transform': {'scaleX': 1, 'scaleY': 1}, 'opacity': 1,
             'fill': {'kind': 'solid', 'color': '#FFE082'}}
    added = patch({'expectedRevision': revision,
                   'layers': [{'layer': spark, 'parentId': 'inset'}], 'order': [
                       {'parentId': 'inset', 'layerIds': ['facet', 'spark']}]})
    assert [c['id'] for c in layer(added['scene']['document'], 'inset')['children']] == ['facet', 'spark']
    revision = added['scene']['revision']
    status, failed = demo.raw('PATCH', '/v1/scenes/emblem', {'expectedRevision': revision,
                          'layers': [{'layer': layer(added['scene']['document'], 'inset'), 'parentId': 'facet'}]})
    assert status == 400, failed
    moved = patch({'expectedRevision': revision,
                   'layers': [{'layer': layer(added['scene']['document'], 'spark'), 'parentId': ''}]})
    assert order(moved['scene']['document']) == ['accent', 'plate', 'inset', 'glow', 'spark']
    revision = moved['scene']['revision']

    # Two editors creating the same new layer id with different content cannot
    # be combined; the committed layer stays untouched.
    rival = dict(spark, id='badge', fill={'kind': 'solid', 'color': '#111111'})
    opponent = dict(spark, id='badge', fill={'kind': 'solid', 'color': '#222222'})
    first_badge = patch({'expectedRevision': revision, 'layers': [{'layer': rival, 'parentId': 'inset'}]})
    revision = first_badge['scene']['revision']
    status, failed = demo.raw('PATCH', '/v1/scenes/emblem', {'expectedRevision': revision - 1,
                          'layers': [{'layer': opponent, 'parentId': 'inset'}]})
    assert status == 409 and failed['details']['conflicts'][0]['reason'] == 'layer-created', failed

    # Parallel editors all branch from the same revision. The store commits one
    # edit at a time and the merge must preserve every contributor.
    start = demo.call('GET', '/v1/scenes/emblem')
    stripe_ids = [f'stripe-{i}' for i in range(6)]
    edits = [{'expectedRevision': start['revision'],
              'layers': [{'layer': {'id': ident, 'kind': 'rectangle', 'width': 8, 'height': 8,
                                    'transform': {'scaleX': 1, 'scaleY': 1}, 'opacity': .4,
                                    'fill': {'kind': 'solid', 'color': '#%02X%02X40' % (32 + 24 * i, 40)}}}]}
             for i, ident in enumerate(stripe_ids)]
    with ThreadPoolExecutor(max_workers=len(edits)) as pool:
        outcomes = list(pool.map(lambda edit: demo.raw('PATCH', '/v1/scenes/emblem', edit), edits))
    assert all(status == 200 for status, _ in outcomes), outcomes
    parallel = demo.call('GET', '/v1/scenes/emblem')
    assert all(layer(parallel['document'], ident) for ident in stripe_ids), 'concurrent edit was lost'
    assert revision + len(edits) == parallel['revision']
    revision = parallel['revision']

    # Two freeze-with-edit requests race from the same observed revision: the
    # winner commits one merged revision and one snapshot, the loser is
    # rejected instead of freezing a revision it never saw.
    before = snapshots()
    race_target = layer(parallel['document'], stripe_ids[0])

    def freeze_once(color):
        edit = {'expectedRevision': revision,
                'layers': [{'layer': dict(race_target, fill={'kind': 'solid', 'color': color})}]}
        return demo.raw('POST', '/v1/scenes/emblem/snapshots', {'expectedRevision': revision, 'edit': edit})

    with ThreadPoolExecutor(max_workers=2) as pool:
        outcomes = list(pool.map(freeze_once, ['#010203', '#040506']))
    statuses = sorted(status for status, _ in outcomes)
    assert statuses == [201, 409], outcomes
    assert snapshots() == before + 1, 'racing freezes produced more than one snapshot'
    revision += 1
    assert demo.call('GET', '/v1/scenes/emblem')['revision'] == revision
    winner = next(body for status, body in outcomes if status == 201)
    assert winner['revision'] == revision and winner['document'] == demo.call('GET', '/v1/scenes/emblem')['document']

    # The unconditional whole-document write still rejects stale bases.
    status, failed = demo.raw('PUT', '/v1/scenes/emblem', {'name': '旧版本', 'expectedRevision': 1, 'document': d})
    assert status == 409

    # Merge and freeze a single revision in one transaction.
    tinted = dict(layer(demo.call('GET', '/v1/scenes/emblem')['document'], 'plate'),
                  fill={'kind': 'linear', 'color': '#000000', 'endColor': '#FFFFFF', 'from': {'x': 0, 'y': 0}, 'to': {'x': 220, 'y': 150}})
    frozen = demo.call('POST', '/v1/scenes/emblem/snapshots', {'expectedRevision': revision,
                        'edit': {'expectedRevision': revision, 'layers': [{'layer': tinted}]}}, 201)
    final = demo.call('GET', '/v1/scenes/emblem')
    revision = final['revision']
    assert frozen['revision'] == revision
    assert frozen['document'] == final['document'], 'snapshot mixed two revisions'
    assert layer(frozen['document'], 'accent')['fill']['color'] == '#223344'
    assert layer(frozen['document'], 'facet')['opacity'] == .6
    assert frozen['digest'] == demo.call('GET', '/v1/snapshots/' + frozen['id'])['digest']
    job = demo.wait(demo.submit(frozen)['id'])
    png = demo.call('GET', '/v1/artifacts/' + job['artifactId'])
    assert png[:8] == b'\x89PNG\r\n\x1a\n'

    # A conflicting edit freezes nothing and keeps the other editor's change.
    before = snapshots()
    base_glow = layer(demo.call('GET', '/v1/scenes/emblem')['document'], 'glow')
    prepared = dict(base_glow, opacity=.75)
    committed = dict(base_glow, fill={'kind': 'solid', 'color': '#ABCABC'})
    demo.call('PATCH', '/v1/scenes/emblem', {'expectedRevision': revision, 'layers': [{'layer': committed}]})
    revision += 1
    status, failed = demo.raw('POST', '/v1/scenes/emblem/snapshots', {'expectedRevision': revision,
                          'edit': {'expectedRevision': revision - 1, 'layers': [{'layer': prepared}]}})
    assert status == 409 and failed['details']['conflicts'][0]['layer'] == 'glow', failed
    assert snapshots() == before, 'conflicting freeze left a snapshot behind'
    assert demo.call('GET', '/v1/scenes/emblem')['revision'] == revision
    assert layer(demo.call('GET', '/v1/scenes/emblem')['document'], 'glow')['fill']['color'] == '#ABCABC'
    assert layer(demo.call('GET', '/v1/scenes/emblem')['document'], 'glow')['opacity'] == .5

    # The same edit rebased on the committed revision freezes one new revision.
    ready = dict(layer(demo.call('GET', '/v1/scenes/emblem')['document'], 'glow'), opacity=.75)
    frozen = demo.call('POST', '/v1/scenes/emblem/snapshots', {'expectedRevision': revision,
                        'edit': {'expectedRevision': revision, 'layers': [{'layer': ready}]}}, 201)
    revision += 1
    assert frozen['revision'] == revision and frozen['document'] == demo.call('GET', '/v1/scenes/emblem')['document']

    # A plain freeze of a stale revision is rejected instead of capturing a
    # revision the caller never saw.
    demo.call('POST', '/v1/scenes/emblem/snapshots', {'expectedRevision': revision - 1}, 409)
    assert demo.call('GET', '/v1/scenes/emblem')['revision'] == revision

    # The whole-document path remains available for complete replacements.
    updated = demo.call('PUT', '/v1/scenes/emblem', {'name': '双色印记',
                        'expectedRevision': revision, 'document': dict(final['document'], background='#101010')})
    assert updated['revision'] == revision + 1 and updated['document']['background'] == '#101010'
    events = demo.call('GET', '/v1/events')
    assert any(e['kind'] == 'scene.edited' for e in events)
    return {'workflow': 'merge-scene-edits', 'state': job['state'], 'revision': updated['revision'],
            'snapshotRevision': frozen['revision'], 'conflicts': 5, 'pngBytes': len(png)}


def run(demo, workflow, out):
    d = sample()
    put = lambda doc, rev=0: demo.call('PUT', '/v1/scenes/emblem', {'name': '双色印记', 'expectedRevision': rev, 'document': doc})
    scene = put(d)
    if workflow == 'merge-scene-edits':
        return merge_scene_edits(demo, d, put)
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
