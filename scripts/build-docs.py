#!/usr/bin/env python3
"""Build the explicitly curated Global VPC documentation site using the stdlib."""
from __future__ import annotations

import argparse
import hashlib
import html
import json
import os
from pathlib import Path
import re
import shutil
import tempfile
from html.parser import HTMLParser
from urllib.parse import quote, unquote, urlsplit

ROOT = Path(__file__).resolve().parents[1]
CONTENT = ROOT / "docs/site/content"
PAGES = [
    ("Start here", "Overview", "index", CONTENT / "index.md"),
    ("Start here", "Installation", "installation", ROOT / "docs/managed-quickstart.md"),
    ("Start here", "vpcctl CLI", "vpcctl", ROOT / "docs/vpcctl.md"),
    ("Start here", "Configuration", "configuration", CONTENT / "configuration.md"),
    ("Design", "Architecture", "architecture", CONTENT / "architecture.md"),
    ("Design", "Kube-OVN integration", "integration", CONTENT / "integration.md"),
    ("Design", "Packet path & transports", "transports", CONTENT / "transports.md"),
    ("Design", "HA & recovery", "ha", CONTENT / "ha.md"),
    ("Operate", "Operations", "operations", CONTENT / "operations.md"),
    ("Operate", "API reference", "api", CONTENT / "api.md"),
    ("Operate", "Validation & limits", "validation", CONTENT / "validation.md"),
    ("Learn", "圖解 · Engineering briefing", "diagrams", CONTENT / "diagrams.md"),
    ("Learn", "Native extension contract", "native-extension", ROOT / "integration/kube-ovn/README.md"),
    ("Learn", "v1.16.3 adapter", "native-compat", ROOT / "integration/kube-ovn/compat-v1.16.3/README.md"),
    ("Maintainers", "Kube-OVN patch & recovery", "native-installation", ROOT / "docs/kube-ovn-extension-install.md"),
    ("Project", "Release readiness", "release-readiness", CONTENT / "release-readiness.md"),
    ("Project", "Release guide", "releasing", ROOT / "docs/releasing.md"),
    ("Project", "Changelog", "changelog", ROOT / "CHANGELOG.md"),
    ("Project", "Contributing", "contributing", ROOT / "CONTRIBUTING.md"),
    ("Project", "Security", "security", ROOT / "SECURITY.md"),
]
DOWNLOADS = [
    "config/examples/helm/authority-dc-a.yaml", "config/examples/helm/authority.yaml", "config/examples/helm/native.yaml",
    "config/examples/helm/site-a.yaml", "config/examples/helm/site-b.yaml",
    "config/examples/managed/network.yaml", "config/examples/managed/platform.json",
    "config/examples/managed/site-a.json", "config/examples/managed/site-b.json",
    "config/examples/managed/smoke-pod.yaml", "config/managed/authority.yaml",
    "config/managed/site.yaml", "config/managed/location-access.yaml",
    "config/managed/project-role.yaml", "config/crd/platform.globalvpc.io_vpcs.yaml",
    "config/crd/platform.globalvpc.io_subnets.yaml",
    "config/crd/platform.globalvpc.io_networkbindings.yaml", "api/v1alpha2/types.go",
    "integration/kube-ovn/example-vpc.yaml", "integration/kube-ovn/source-lock.json",
    "integration/kube-ovn/compat-v1.16.3/source-lock.json",
    "LICENSE", "NOTICE", "docs/managed-solution-diagrams/speaker-notes.zh-TW.md",
]
# This is a closed publication list. Never walk the repository to copy content.
FORBIDDEN = ("artifacts/", "global-vpc-controller-handoff/", ".local/", ".git/")


def esc(value):
    return html.escape(str(value), quote=True)


def slug(text):
    text = re.sub(r"[`*_]", "", text).lower().strip()
    return re.sub(r"[^\w\-\s]", "", text, flags=re.UNICODE).replace(" ", "-")


class Renderer:
    def __init__(self, source, locations, repository, ref):
        self.source = source
        self.locations = locations
        self.repository = repository
        self.ref = ref
        self.headings = []
        self.ids = {}

    def destination(self, href):
        parts = urlsplit(href)
        if parts.scheme:
            return href if parts.scheme in ("https", "http", "mailto") else None
        if href.startswith("//"):
            return None
        if not parts.path:
            return href
        path = (self.source.parent / unquote(parts.path)).resolve()
        try:
            relative = path.relative_to(ROOT).as_posix()
        except ValueError:
            return None
        if any(relative.startswith(item) for item in FORBIDDEN):
            raise ValueError(f"Private source reference is not publishable: {self.source.relative_to(ROOT)}")
        suffix = ("#" + parts.fragment) if parts.fragment else ""
        if path in self.locations:
            return self.locations[path] + suffix
        if path.exists():
            kind = "tree" if path.is_dir() else "blob"
            return f"https://github.com/{self.repository}/{kind}/{quote(self.ref, safe='')}/{quote(relative)}{suffix}"
        raise ValueError(f"Broken source link: {self.source.relative_to(ROOT)} -> {href}")

    def inline(self, text):
        tokens = []
        def save(value):
            tokens.append(value)
            return f"\x00{len(tokens)-1}\x00"
        text = re.sub(r"`([^`]+)`", lambda m: save(f"<code>{esc(m[1])}</code>"), text)
        def link(m):
            is_image, label, href = m[1], m[2], m[3]
            dest = self.destination(href)
            if dest is None:
                return save(esc(label))
            if is_image:
                return save(f'<a class="figure-link" href="{esc(dest)}"><img loading="lazy" src="{esc(dest)}" alt="{esc(label)}"></a>')
            return save(f'<a href="{esc(dest)}">{esc(label)}</a>')
        text = re.sub(r"(!?)\[([^\]]+)\]\(([^\s)]+)\)", link, text)
        text = esc(text)
        text = re.sub(r"\*\*(.+?)\*\*", r"<strong>\1</strong>", text)
        text = re.sub(r"(?<!\*)\*([^*]+)\*(?!\*)", r"<em>\1</em>", text)
        for i, value in enumerate(tokens):
            text = text.replace(f"\x00{i}\x00", value)
        return text

    def render(self, markdown):
        lines = markdown.splitlines()
        out, paragraph, listing = [], [], None
        def flush():
            if paragraph:
                out.append("<p>" + self.inline(" ".join(paragraph)) + "</p>")
                paragraph.clear()
        def close_list():
            nonlocal listing
            if listing:
                out.append(f"</{listing}>")
                listing = None
        i = 0
        while i < len(lines):
            line = lines[i]
            if line.startswith("```"):
                flush(); close_list()
                language = line[3:].strip()
                code = []; i += 1
                while i < len(lines) and not lines[i].startswith("```"):
                    code.append(lines[i]); i += 1
                out.append(f'<div class="code-block"><div class="code-label">{esc(language or "text")}</div><pre><code>{esc(chr(10).join(code))}</code></pre></div>')
            elif re.match(r"^#{1,6} ", line):
                flush(); close_list()
                level, title = line.split(" ", 1)
                ident = slug(title)
                count = self.ids.get(ident, 0); self.ids[ident] = count + 1
                if count:
                    ident += f"-{count}"
                self.headings.append((len(level), title, ident))
                out.append(f'<h{len(level)} id="{esc(ident)}">{self.inline(title)}<a class="anchor" href="#{esc(ident)}" aria-label="Link to this section">#</a></h{len(level)}>')
            elif line.startswith("|") and i + 1 < len(lines) and re.match(r"^\|[\s:|\-]+\|?$", lines[i+1]):
                flush(); close_list()
                row = lambda v: [item.strip() for item in v.strip().strip("|").split("|")]
                head = row(line); i += 2
                out.append('<div class="table-wrap" tabindex="0" role="region" aria-label="Scrollable reference table"><table><thead><tr>' + ''.join(f'<th scope="col">{self.inline(v)}</th>' for v in head) + '</tr></thead><tbody>')
                while i < len(lines) and lines[i].startswith("|"):
                    out.append('<tr>' + ''.join(f'<td>{self.inline(v)}</td>' for v in row(lines[i])) + '</tr>'); i += 1
                out.append('</tbody></table></div>'); continue
            elif line.startswith("> "):
                flush(); close_list()
                quote_lines = []
                while i < len(lines) and lines[i].startswith("> "):
                    quote_lines.append(lines[i][2:]); i += 1
                out.append('<aside class="callout">' + self.inline(' '.join(quote_lines)) + '</aside>'); continue
            elif re.match(r"^\s*(?:[-*]|\d+\.) ", line):
                flush()
                match = re.match(r"^\s*([-*]|\d+\.) (.*)", line)
                kind = 'ol' if match[1][0].isdigit() else 'ul'
                if listing != kind:
                    close_list(); listing = kind; out.append(f'<{kind}>')
                item = [match[2]]; i += 1
                while i < len(lines) and lines[i].startswith('  ') and not re.match(r"^\s*(?:[-*]|\d+\.) ", lines[i]):
                    item.append(lines[i].strip()); i += 1
                out.append('<li>' + self.inline(' '.join(item)) + '</li>'); continue
            elif line.strip() == "<!-- DIAGRAM_GALLERY -->":
                flush(); close_list(); out.append("<!-- DIAGRAM_GALLERY -->")
            elif not line.strip():
                flush(); close_list()
            elif line.strip().startswith('<!--'):
                pass
            else:
                paragraph.append(line.strip())
            i += 1
        flush(); close_list()
        return '\n'.join(out)


def gallery(manifest):
    cards = []
    for slide in manifest['slides']:
        stem = slide['slug']
        cards.append(f'''<section class="diagram-card" lang="zh-Hant" id="diagram-{slide['number']:02d}">
<div class="diagram-heading"><span class="diagram-number">{slide['number']:02d}</span><h2>{esc(slide['title'])}</h2></div>
<p>{esc(slide['message'])}</p><a href="assets/diagrams/{esc(slide['svg'])}" class="figure-link"><img loading="lazy" src="assets/diagrams/{esc(slide['svg'])}" alt="{esc(slide['title'])}"></a>
<div class="download-links"><a href="assets/diagrams/{esc(slide['png'])}" download>Download 4K PNG</a><a href="assets/diagrams/{esc(slide['svg'])}" download>Download SVG</a></div>
<details><summary>Speaker notes · 逐圖講解</summary><ol>{''.join('<li>'+esc(note)+'</li>' for note in slide['notes'])}</ol></details></section>''')
    return '\n'.join(cards)


def navigation(current, pages):
    result, previous = [], None
    for group, title, name, _ in pages:
        if group != previous:
            if previous:
                result.append('</ul>')
            result.append(f'<p class="nav-group">{esc(group)}</p><ul>'); previous = group
        state = ' aria-current="page"' if name == current else ''
        result.append(f'<li><a href="{name}.html"{state}>{esc(title)}</a></li>')
    result.append('</ul>')
    return ''.join(result)


def template(title, name, body, renderer, pages, version, repository):
    toc = ''.join(f'<li><a href="#{esc(ident)}">{esc(t)}</a></li>' for level, t, ident in renderer.headings if level == 2)
    position = [p[2] for p in pages].index(name)
    pager = []
    for offset, label in ((-1, 'Previous'), (1, 'Next')):
        n = position + offset
        if 0 <= n < len(pages):
            p = pages[n]; pager.append(f'<a href="{p[2]}.html"><span>{label}</span>{esc(p[1])}</a>')
    source = renderer.source.relative_to(ROOT).as_posix()
    return f'''<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="description" content="Global VPC {esc(version)} documentation: managed multi-location tenant networking for Kube-OVN.">
<meta name="color-scheme" content="light dark"><meta name="theme-color" content="#102a43"><title>{esc(title)} · Global VPC</title>
<link rel="icon" href="assets/favicon.svg" type="image/svg+xml"><link rel="stylesheet" href="assets/site.css">
<script src="assets/search-index.js" defer></script><script src="assets/site.js" defer></script></head>
<body class="page-{name}"><a class="skip-link" href="#content">Skip to content</a>
<header class="topbar"><a class="brand" href="index.html"><img src="assets/favicon.svg" width="32" height="32" alt=""><span>Global VPC</span></a><span class="version">{esc(version)} <span>Experimental</span></span><div class="header-actions"><a href="https://github.com/{repository}" class="repo-link">GitHub ↗</a><button id="theme-toggle" type="button" aria-label="Switch color theme">Theme</button><button id="menu-toggle" type="button" aria-expanded="false" aria-controls="sidebar">Menu</button></div></header>
<div class="layout"><aside id="sidebar" class="sidebar"><label for="search">Search documentation <kbd>/</kbd></label><input id="search" type="search" placeholder="Architecture, BFD, install…" autocomplete="off" aria-controls="search-results"><div id="search-status" class="sr-only" aria-live="polite"></div><div id="search-results" hidden></div><nav aria-label="Documentation">{navigation(name,pages)}</nav><p class="sidebar-foot">Native Kube-OVN integration.<br>Independent open-source project.</p></aside>
<main id="content" tabindex="-1"><div class="eyebrow">{esc(pages[position][0])} / Managed networking</div><article>{body}</article><nav class="pager" aria-label="Page navigation">{''.join(pager)}</nav><footer><a href="https://github.com/{repository}/blob/{quote(renderer.ref, safe='')}/{quote(source)}">Page source ↗</a><span><a href="downloads/LICENSE">Apache-2.0</a> · {esc(version)} · API v1alpha2</span></footer></main>
<aside class="toc"><p>On this page</p><nav aria-label="On this page"><ul>{toc}</ul></nav><div class="toc-note">Alpha API<br>Source-pinned native extension<br>Qualify before production</div></aside></div></body></html>'''


class LinkParser(HTMLParser):
    def __init__(self):
        super().__init__(); self.links = []; self.ids = set()
    def handle_starttag(self, tag, attrs):
        values = dict(attrs)
        if 'id' in values: self.ids.add(values['id'])
        for key in ('href', 'src'):
            if key in values: self.links.append(values[key])


def validate(out, pages):
    parsed = {}
    for path in out.glob('*.html'):
        parser = LinkParser(); parser.feed(path.read_text()); parsed[path] = parser
    errors = []
    for path, parser in parsed.items():
        for href in parser.links:
            part = urlsplit(href)
            if part.scheme or href.startswith('//'): continue
            target = (path.parent / unquote(part.path)).resolve() if part.path else path
            if not target.exists(): errors.append(f'{path.name}: missing {href}'); continue
            if part.fragment and target in parsed and unquote(part.fragment) not in parsed[target].ids:
                errors.append(f'{path.name}: missing anchor {href}')
    for _, _, name, _ in pages:
        path = out / f'{name}.html'
        if path.stat().st_size < 1000: errors.append(f'Empty page: {name}')
    for path in out.rglob('*'):
        relative = path.relative_to(out).as_posix()
        if any(relative.startswith(item) for item in FORBIDDEN): errors.append(f'Forbidden output: {relative}')
    if errors: raise ValueError('\n'.join(errors))
    return {'pages':len(pages),'htmlFiles':len(parsed),'internalLinksChecked':sum(len(p.links) for p in parsed.values()),'status':'PASS'}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', default='public')
    parser.add_argument('--repository', default=os.getenv('GITHUB_REPOSITORY', 'yckao/kube-ovn-global-vpc'))
    parser.add_argument('--ref', default='main')
    parser.add_argument('--check', action='store_true', help='Require every curated project page and run publication checks')
    args = parser.parse_args()
    if not re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', args.repository): parser.error('repository must be OWNER/REPOSITORY')
    out = Path(args.output).resolve()
    if out in (ROOT, ROOT.parent, Path('/')) or ROOT in out.parents and '.git' in out.parts: parser.error('unsafe output directory')
    if out.exists() and not (out / '.gvpc-docs-output').exists(): parser.error('output exists without .gvpc-docs-output marker; choose an empty new path')
    version_path = ROOT / 'VERSION'
    version = ('v' + version_path.read_text().strip().removeprefix('v')) if version_path.exists() else 'v0.1.0'
    pages = [p for p in PAGES if p[3].exists()]
    if args.check and len(pages) != len(PAGES): parser.error('missing curated project pages: ' + ', '.join(str(p[3].relative_to(ROOT)) for p in PAGES if not p[3].exists()))
    locations = {p[3].resolve():p[2]+'.html' for p in pages}
    # A generic validation source may be linked by imported quickstart content.
    locations[(ROOT/'docs/managed-validation.md').resolve()] = 'validation.html'
    manifest = json.loads((ROOT/'docs/managed-solution-diagrams/manifest.json').read_text())
    out.parent.mkdir(parents=True,exist_ok=True)
    temp = Path(tempfile.mkdtemp(prefix='.gvpc-docs-',dir=out.parent))
    try:
        (temp/'.gvpc-docs-output').write_text('Generated by scripts/build-docs.py\n')
        (temp/'.nojekyll').write_text('')
        (temp/'assets').mkdir(); (temp/'assets/diagrams').mkdir()
        for name in ('site.css','site.js','favicon.svg'):
            shutil.copy2(ROOT/'docs/site/assets'/name,temp/'assets'/name)
        for relative in DOWNLOADS:
            source=ROOT/relative
            if not source.exists(): continue
            target=temp/'downloads'/relative; target.parent.mkdir(parents=True,exist_ok=True); shutil.copy2(source,target)
            locations[source.resolve()]='downloads/'+relative
        for slide in manifest['slides']:
            for kind in ('svg','png'):
                name=slide[kind]
                if Path(name).name != name or not name.endswith('.'+kind): raise ValueError('unsafe diagram manifest path')
                source=ROOT/'docs/managed-solution-diagrams'/name
                shutil.copy2(source,temp/'assets/diagrams'/name)
                locations[source.resolve()]='assets/diagrams/'+name
        index=[]
        for _, title, name, source in pages:
            markdown=source.read_text()
            renderer=Renderer(source,locations,args.repository,args.ref)
            body=renderer.render(markdown)
            if name=='diagrams':
                body=body.replace('<!-- DIAGRAM_GALLERY -->',gallery(manifest))
                renderer.headings.extend((2, f"{slide['number']:02d} {slide['title']}", f"diagram-{slide['number']:02d}") for slide in manifest['slides'])
            (temp/f'{name}.html').write_text(template(title,name,body,renderer,pages,version,args.repository))
            plain=re.sub(r'<[^>]*>',' ',body)
            index.append({'title':title,'url':name+'.html','text':html.unescape(re.sub(r'\s+',' ',plain)).strip()})
        (temp/'assets/search-index.js').write_text('window.GVPC_SEARCH='+json.dumps(index,ensure_ascii=False).replace('</','<\\/')+';\n')
        # A relative-home fallback works for project Pages without a hard-coded host.
        (temp/'404.html').write_text('<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Page not found · Global VPC</title><body><h1>Page not found</h1><p>Open the <a href="https://'+args.repository.split('/')[0]+'.github.io/'+args.repository.split('/')[1]+'/">documentation home</a> or use your browser to go back.</p></body></html>')
        report=validate(temp,pages)
        report.update({'version':version,'repository':args.repository,'diagramCount':len(manifest['slides']),'searchEntries':len(index),'privateArtifactsIncluded':False})
        inventory={p.relative_to(temp).as_posix():hashlib.sha256(p.read_bytes()).hexdigest() for p in temp.rglob('*') if p.is_file()}
        (temp/'build-manifest.json').write_text(json.dumps({'validation':report,'sha256':inventory},indent=2)+'\n')
        if out.exists(): shutil.rmtree(out)
        temp.rename(out)
        print(json.dumps(report,indent=2))
    finally:
        if temp.exists(): shutil.rmtree(temp)


if __name__=='__main__':
    main()
