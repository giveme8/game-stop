import { sha256 } from '@noble/hashes/sha2.js';
import { md5 } from '@noble/hashes/legacy.js';
import { Env, HttpError, hex } from './types';
import { safePath } from './package';

// R2 range header/directory reads are bounded; payload checks run in one stream.
export async function inspectStoredPCK(env: Env, key: string, size: number): Promise<{engineVersion:string,sha256:string}> {
  const bad = () => new HttpError(400,'invalid or unsafe PCK');
  if (!Number.isSafeInteger(size) || size < 100 || size > 512*1024*1024) throw bad();
  async function range(offset:number,length:number) {
    const object = await env.PACKAGES.get(key,{range:{offset,length}});
    if (!object || !('body' in object)) throw new HttpError(503,'upload storage unavailable');
    const data = new Uint8Array(await object.arrayBuffer()); if (data.length !== length) throw bad(); return data;
  }
  const h = await range(0,Math.min(104,size)); const hv = new DataView(h.buffer);
  const u64 = (v:DataView,p:number) => { const n = v.getBigUint64(p,true); if(n>BigInt(Number.MAX_SAFE_INTEGER)) throw bad(); return Number(n); };
  const format=hv.getUint32(4,true), major=hv.getUint32(8,true), minor=hv.getUint32(12,true);
  if(new TextDecoder('utf-8',{fatal:false,ignoreBOM:false}).decode(h.subarray(0,4))!=='GDPC'||format<2||format>4||major!==4||minor>999||(hv.getUint32(20,true)&~2)!==0) throw bad();
  const base=u64(hv,24), headerEnd=format>=3?104:96, dir=format>=3?u64(hv,32):96;
  if(dir<headerEnd||dir>size-4||base>size) throw bad();
  const directory=await range(dir,Math.min(8*1024*1024,size-dir)); const dv=new DataView(directory.buffer); let pos=4;
  const count=dv.getUint32(0,true); if(count<1||count>10000) throw new HttpError(400,'PCK directory exceeds SaaS limit');
  const entries:Array<{start:number,end:number,sum:string}>=[]; const seen=new Set<string>();
  for(let i=0;i<count;i++) {
    if(pos+4>directory.length) throw bad(); const n=dv.getUint32(pos,true); pos+=4;
    if(!n||n>4096||pos+n+36>directory.length) throw bad();
    let name:string; try { name=new TextDecoder('utf-8',{fatal:true,ignoreBOM:false}).decode(directory.subarray(pos,pos+n)).replace(/\0+$/,''); } catch { throw bad(); }
    pos+=n; const canonical=name.replace(/^res:\/\//,'').toLowerCase();
    if(!safePath(name)||seen.has(canonical)) throw bad(); seen.add(canonical);
    const start=base+u64(dv,pos), length=u64(dv,pos+8); pos+=16;
    const sum=hex(directory.subarray(pos,pos+16)); pos+=16; const flags=dv.getUint32(pos,true); pos+=4;
    if(flags||!Number.isSafeInteger(start+length)||start<headerEnd||start+length>size) throw bad();
    entries.push({start,end:start+length,sum});
  }
  entries.sort((a,b)=>a.start-b.start); let prev=0;
  for(const e of entries) { if(e.start<prev||(e.start<dir+pos&&e.end>dir)) throw bad(); prev=e.end; }
  const object=await env.PACKAGES.get(key); if(!object||!('body' in object)) throw bad();
  const whole=sha256.create(); let file=md5.create(), index=0, offset=0;
  const reader=object.body.getReader();
  try { for(;;) {
    const {done,value}=await reader.read(); if(done) break; whole.update(value); const end=offset+value.length;
    if(end>size) throw bad();
    while(index<entries.length) {
      const e=entries[index];
      if(e.start>=end && e.start!==e.end) break;
      const lo=Math.max(offset,e.start), hi=Math.min(end,e.end);
      if(hi>lo) file.update(value.subarray(lo-offset,hi-offset));
      if(e.end>end) break;
      if(hex(file.digest())!==e.sum) throw bad(); index++; file=md5.create();
    }
    offset=end;
  }} catch(error) { await reader.cancel(); throw error; } finally { reader.releaseLock(); }
  while(index<entries.length && entries[index].start===size && entries[index].end===size) {
    if(hex(file.digest())!==entries[index].sum) throw bad(); index++; file=md5.create();
  }
  if(offset!==size||index!==entries.length) throw bad();
  return {engineVersion:`${major}.${minor}`,sha256:hex(whole.digest())};
}
