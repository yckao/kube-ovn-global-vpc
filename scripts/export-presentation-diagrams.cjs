#!/usr/bin/env node
/* Rasterize authored SVGs with Chromium and report text-layout problems.
 * Usage: node scripts/export-presentation-diagrams.cjs [playwright module] [browser executable] [output directory]
 */
const fs = require('fs');
const path = require('path');
const { chromium } = require(process.argv[2] || 'playwright');
const root = path.resolve(__dirname, '..');
const out = process.argv[4] ? path.resolve(process.argv[4]) : path.join(root, 'docs/managed-solution-diagrams');
const qa = path.join(root, 'artifacts', path.basename(out));

(async () => {
  fs.mkdirSync(qa, {recursive: true});
  const browser = await chromium.launch({headless: true, ...(process.argv[3] ? {executablePath: process.argv[3]} : {})});
  const page = await browser.newPage({viewport: {width: 1920, height: 1080}, deviceScaleFactor: 2});
  const report = [];
  for (const name of fs.readdirSync(out).filter(n => n.endsWith('.svg')).sort()) {
    const svg = fs.readFileSync(path.join(out, name), 'utf8');
    await page.setContent(`<html><head><style>html,body{margin:0;width:1920px;height:1080px;overflow:hidden}svg{display:block}</style></head><body>${svg}</body></html>`);
    await page.evaluate(() => document.fonts.ready);
    const layout = await page.evaluate(() => {
      const texts = [...document.querySelectorAll('svg text')].map(el => {
        const b = el.getBBox();
        return {text: el.textContent, x:b.x,y:b.y,w:b.width,h:b.height,maxWidth:Number(el.dataset.maxWidth)||null};
      });
      const overflow = texts.filter(b => b.x < 0 || b.y < 0 || b.x+b.w > 1920 || b.y+b.h > 1080 || (b.maxWidth && b.w>b.maxWidth+1));
      const overlaps = [];
      for (let i=0;i<texts.length;i++) for(let j=i+1;j<texts.length;j++) {
        const a=texts[i], b=texts[j];
        if (Math.min(a.x+a.w,b.x+b.w)-Math.max(a.x,b.x)>2 && Math.min(a.y+a.h,b.y+b.h)-Math.max(a.y,b.y)>2) overlaps.push([a.text,b.text]);
      }
      return {textCount:texts.length,overflow,overlaps};
    });
    await page.screenshot({path:path.join(out,name.replace('.svg','.png')),fullPage:false});
    report.push({name, width:3840, height:2160, ...layout});
    console.log(JSON.stringify(report[report.length-1]));
  }
  fs.writeFileSync(path.join(qa,'render-qa.json'), JSON.stringify(report,null,2)+'\n');
  await browser.close();
  if (report.some(r => r.overflow.length || r.overlaps.length)) process.exitCode=1;
})().catch(e => {console.error(e.message);process.exitCode=1});
