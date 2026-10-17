// Protocol observations and chart arithmetic are independent of DOM and timers.
globalThis.FlowScreenModel = (() => {
  const channels = Object.freeze(['workbuddy', 'cline', 'qoder', 'grok']);
  const protocols = Object.freeze(['chat', 'responses', 'messages']);
  const keys = Object.freeze([...channels, 'gateway', ...protocols]);
  const names = Object.freeze({ workbuddy:'WorkBuddy', cline:'Cline', qoder:'Qoder', grok:'Grok', gateway:'API Console', chat:'Chat Completions', responses:'Responses', messages:'Messages' });
  function numeric(value) { return value != null && value !== '' && Number.isFinite(Number(value)) && Number(value) >= 0 ? Number(value) : null; }
  function number(value, digits = 0) { const n=numeric(value); return n == null ? '—' : n.toLocaleString('en-US',{minimumFractionDigits:digits,maximumFractionDigits:digits}); }
  function milliseconds(value) { const n=numeric(value); return n == null ? '暂无样本' : n < 1000 ? Math.round(n)+' ms' : (n/1000).toFixed(1)+' s'; }
  function percent(value) { const n=numeric(value); return n == null ? '暂无样本' : (n*100).toFixed(1)+'%'; }
  function sum(rows, field) { return rows.reduce((total,row)=>total+(numeric(row[field])??0),0); }
  function normalize(payload, scope) {
    if (!payload || payload.available !== true || !Array.isArray(payload.routes) || !payload.routes.length || typeof payload.instance_started !== 'string' || !payload.instance_started || !Number.isFinite(Date.parse(payload.sampled_at))) return null;
    const seen=new Set(), routes=[];
    for (const row of payload.routes) {
      if (!channels.includes(row.channel) || !protocols.includes(row.protocol)) return null;
      if (['active','started','completed','qps'].some(key=>numeric(row[key])==null)) return null;
      const id=row.channel+':'+row.protocol;
      if (seen.has(id)) return null;
      seen.add(id);
      if (!scope || row.channel===scope) routes.push({...row});
    }
    const scopedChannels=scope?[scope]:channels;
    if (routes.length!==scopedChannels.length*protocols.length || !payload.nodes || [...scopedChannels,'gateway',...protocols].some(key=>!payload.nodes[key])) return null;
    return {...payload,routes};
  }
  function rowsFor(snapshot,key) { return (snapshot?.routes||[]).filter(row=>key==='gateway'||row.channel===key||row.protocol===key); }
  function rate(rows) {
    const known=rows.length>0 && rows.every(row=>row.usage_available===true && numeric(row.output_tps)!=null);
    return { qps:sum(rows,'qps'), active:sum(rows,'active'), output:known?sum(rows,'output_tps'):null };
  }
  function deltas(previous, snapshot) {
    const result=new Map(keys.map(key=>[key,{incoming:0,outgoing:0}]));
    if (!previous || previous.instance_started!==snapshot.instance_started) return result;
    const old=new Map(previous.routes.map(row=>[row.channel+':'+row.protocol,row]));
    for(const row of snapshot.routes) {
      const prior=old.get(row.channel+':'+row.protocol); if(!prior) continue;
      for(const key of [row.channel,row.protocol,'gateway']) {
        const delta=result.get(key);
        delta.incoming+=Math.max(0,row.started-prior.started);
        delta.outgoing+=Math.max(0,row.completed-prior.completed);
      }
    }
    return result;
  }
  function pool(snapshot,key,scope,now,receivedAt) {
    const rows=(snapshot?.rows||[]).filter(row=>channels.includes(row.channel) && (key==='gateway'?(!scope||row.channel===scope):row.channel===key));
    const expected=key==='gateway'&&!scope?channels.length:1;
    const sampled=Date.parse(snapshot?.sampled_at);
    const fresh=snapshot?.available===true && rows.length===expected && Number.isFinite(sampled) && now-sampled>=-30000 && now-sampled<30000 && now-receivedAt<30000 && rows.every(row=>numeric(row.accounts_available)!=null&&numeric(row.accounts_enabled)!=null);
    const capacityKnown=fresh && rows.every(row=>row.capacity_available===true && numeric(row.capacity_slots)!=null && numeric(row.occupied_slots)!=null);
    return {fresh,capacityKnown,available:fresh?sum(rows,'accounts_available'):null,enabled:fresh?sum(rows,'accounts_enabled'):null,
      capacity:capacityKnown?sum(rows,'capacity_slots'):null,occupied:capacityKnown?sum(rows,'occupied_slots'):null,
      modelCooldowns:fresh?sum(rows,'model_cooldowns'):null,sampledAt:snapshot?.sampled_at};
  }
  class Trend {
    constructor(){this.clear();}
    clear(){this.points=new Map();this.last=null;}
    add(now,point) {
      const second=Math.floor(now/1000);
      if(this.last!=null && second<this.last) this.clear();
      if(this.last!=null) for(let key=Math.max(this.last+1,second-59);key<second;key++)this.points.set(key,null);
      this.points.set(second,point);this.last=second;
      for(const key of this.points.keys()) if(key<second-59)this.points.delete(key);
    }
    values(now){const second=Math.floor(now/1000);return Array.from({length:60},(_,i)=>this.points.get(second-59+i)??null);}
    path(field,now) {
      const points=this.values(now),valid=points.map(point=>numeric(point?.[field])).filter(value=>value!=null);
      const ceiling=Math.max(1,...valid)*1.1;let drawing=false;
      return points.map((point,i)=>{const value=numeric(point?.[field]);if(value==null){drawing=false;return '';}const cmd=drawing?'L':'M';drawing=true;return cmd+(i*360/59).toFixed(2)+','+(56-value/ceiling*48).toFixed(2);}).join(' ');
    }
  }
  return Object.freeze({channels,protocols,keys,names,numeric,number,milliseconds,percent,sum,normalize,rowsFor,rate,deltas,pool,Trend});
})();
