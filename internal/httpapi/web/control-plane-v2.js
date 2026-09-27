/* NexaRoute Control Plane v2
   UX composition layer over the existing admin API. The Go backend remains the
   only configuration/routing authority; every mutation is followed by refresh(). */
'use strict';

(() => {
  const UI = window.NexaUI = window.NexaUI || {};
  const state = {
    lang: localStorage.getItem('nexaroute_lang') || 'en',
    theme: localStorage.getItem('nexaroute_theme') || 'dark',
    providerStep: 0,
    providerSearch: '',
    modelSearch: '',
    modelAdvanced: false,
    routeEditing: null,
    autoProtocol: true
  };

  const tr = {
    en: {
      overview:'Overview', providers:'Providers', routing:'Routing', models:'Models', observability:'Observability', health:'Health', connect:'Connect', settings:'Settings',
      product:'Control Plane', addProvider:'Add provider', addRoute:'Create route', searchProviders:'Search providers…',
      welcome:'Connect your first AI provider', welcomeCopy:'Add an endpoint and credential, detect models, choose what you want to use, then create a stable route for Claude Code or any compatible client.',
      connectProvider:'Add your first provider', createRoute:'Create your first route', ready:'Ready', notReady:'Not ready',
      connection:'Connection', modelsStep:'Models', verify:'Verify', back:'Back', next:'Continue', saveProvider:'Save provider',
      customProvider:'Custom Provider', protocol:'Protocol', protocolAuto:'Auto detect (recommended)', protocolManual:'Manual',
      savedCredential:'Saved credential', replace:'Replace', keepSaved:'Kept unless you replace it',
      detectModels:'Detect models', detecting:'Detecting provider and models…', found:'models found', selectAll:'Select all', clearAll:'Clear all',
      selectVisible:'Select visible', selected:'selected', searchModels:'Search models…', advancedModels:'Advanced model settings',
      routeName:'Route name', publicModel:'Public model', routingMode:'Routing mode', automatic:'Automatic', ordered:'Ordered fallback',
      automaticHelp:'Selected models share one pool. NexaRoute chooses among healthy eligible deployments.',
      orderedHelp:'Each selected deployment becomes an ordered fallback stage; health and compatibility still gate every attempt.',
      chooseModels:'Choose models/deployments', saveRoute:'Save route', edit:'Edit', delete:'Delete', cancel:'Cancel',
      advanced:'Advanced', advancedRouting:'Advanced routing', candidatePools:'Candidate pools', routeProfiles:'Route profiles', endpoints:'Virtual endpoints', fallbackChains:'Fallback chains',
      confirmDelete:'Confirm deletion', deleteProvider:'Delete provider', deleteRoute:'Delete route', destructive:'This action changes the saved NexaRoute configuration.',
      adminKey:'Admin key required', adminKeyCopy:'Enter the x-admin-key for this browser session. It will be stored only in sessionStorage.',
      save:'Save', name:'Name', id:'ID', baseUrl:'Base URL', apiKey:'API Key', enabled:'Enabled', providerType:'Provider type',
      testConnection:'Test connection', testModels:'Test selected models', manualModel:'Add model manually',
      simpleRouting:'Simple routing', advancedRoutingCopy:'Power users can still edit the backend primitives directly. Simple routes are compiled into the same Candidate Pool → Route Profile → Virtual Endpoint objects.',
      noRoutes:'No routes yet', noRoutesCopy:'Create a stable public model such as “coding” and choose the upstream models NexaRoute may use.',
      noProviders:'No providers yet', noProvidersCopy:'Start with a Base URL and API key. NexaRoute can discover the available models for you.',
      themeLight:'Light', themeDark:'Dark', language:'فارسی',
      saved:'Saved', saving:'Saving…', failed:'Failed', success:'Success',
      allEligible:'All eligible deployments', explicit:'Selected deployments', mode:'Mode', pools:'Pools in order',
      displayName:'Display name', fallback:'Fallback chain', pool:'Candidate pool', profile:'Route profile'
    },
    fa: {
      overview:'نمای کلی', providers:'ارائه‌دهنده‌ها', routing:'مسیریابی', models:'مدل‌ها', observability:'نظارت زنده', health:'سلامت', connect:'اتصال', settings:'تنظیمات',
      product:'مرکز کنترل', addProvider:'افزودن ارائه‌دهنده', addRoute:'ساخت مسیر', searchProviders:'جستجوی ارائه‌دهنده…',
      welcome:'اولین ارائه‌دهندهٔ هوش مصنوعی را وصل کنید', welcomeCopy:'آدرس API و کلید را وارد کنید، مدل‌ها را شناسایی و انتخاب کنید، سپس یک مسیر پایدار برای Claude Code یا هر کلاینت سازگار بسازید.',
      connectProvider:'افزودن اولین ارائه‌دهنده', createRoute:'ساخت اولین مسیر', ready:'آماده', notReady:'آماده نیست',
      connection:'اتصال', modelsStep:'مدل‌ها', verify:'بررسی', back:'قبلی', next:'ادامه', saveProvider:'ذخیره ارائه‌دهنده',
      customProvider:'ارائه‌دهنده سفارشی', protocol:'پروتکل', protocolAuto:'تشخیص خودکار (پیشنهادی)', protocolManual:'دستی',
      savedCredential:'کلید ذخیره شده', replace:'تعویض', keepSaved:'تا وقتی تعویض نکنید حفظ می‌شود',
      detectModels:'شناسایی مدل‌ها', detecting:'در حال شناسایی پروتکل و مدل‌ها…', found:'مدل پیدا شد', selectAll:'انتخاب همه', clearAll:'پاک‌کردن همه',
      selectVisible:'انتخاب موارد نمایان', selected:'انتخاب شده', searchModels:'جستجوی مدل…', advancedModels:'تنظیمات پیشرفته مدل',
      routeName:'نام مسیر', publicModel:'نام عمومی مدل', routingMode:'حالت مسیریابی', automatic:'خودکار', ordered:'فال‌بک ترتیبی',
      automaticHelp:'مدل‌های انتخابی در یک Pool قرار می‌گیرند و NexaRoute از میان گزینه‌های سالم و سازگار انتخاب می‌کند.',
      orderedHelp:'هر مدل انتخابی یک مرحلهٔ فال‌بک می‌شود؛ سلامت و سازگاری همچنان قبل از هر انتخاب بررسی می‌شود.',
      chooseModels:'انتخاب مدل‌ها / Deploymentها', saveRoute:'ذخیره مسیر', edit:'ویرایش', delete:'حذف', cancel:'لغو',
      advanced:'پیشرفته', advancedRouting:'مسیریابی پیشرفته', candidatePools:'Candidate Poolها', routeProfiles:'Route Profileها', endpoints:'Virtual Endpointها', fallbackChains:'Fallback Chainها',
      confirmDelete:'تأیید حذف', deleteProvider:'حذف ارائه‌دهنده', deleteRoute:'حذف مسیر', destructive:'این عملیات تنظیمات ذخیره‌شده NexaRoute را تغییر می‌دهد.',
      adminKey:'کلید مدیریت لازم است', adminKeyCopy:'کلید x-admin-key را برای همین نشست مرورگر وارد کنید. فقط در sessionStorage نگهداری می‌شود.',
      save:'ذخیره', name:'نام', id:'شناسه', baseUrl:'آدرس پایه', apiKey:'کلید API', enabled:'فعال', providerType:'نوع ارائه‌دهنده',
      testConnection:'آزمایش اتصال', testModels:'آزمایش مدل‌های انتخابی', manualModel:'افزودن مدل دستی',
      simpleRouting:'مسیریابی ساده', advancedRoutingCopy:'کاربر حرفه‌ای همچنان می‌تواند اجزای اصلی بک‌اند را مستقیم مدیریت کند. مسیر ساده نیز در نهایت همان Candidate Pool → Route Profile → Virtual Endpoint را می‌سازد.',
      noRoutes:'هنوز مسیری ساخته نشده', noRoutesCopy:'یک نام عمومی ثابت مثل «coding» بسازید و مدل‌هایی را که NexaRoute اجازه دارد استفاده کند انتخاب کنید.',
      noProviders:'هنوز ارائه‌دهنده‌ای نیست', noProvidersCopy:'با Base URL و API Key شروع کنید. NexaRoute می‌تواند مدل‌های موجود را برای شما شناسایی کند.',
      themeLight:'روشن', themeDark:'تیره', language:'English',
      saved:'ذخیره شد', saving:'در حال ذخیره…', failed:'ناموفق', success:'موفق',
      allEligible:'همه Deploymentهای واجد شرایط', explicit:'Deploymentهای انتخابی', mode:'حالت', pools:'Poolها به ترتیب',
      displayName:'نام نمایشی', fallback:'Fallback Chain', pool:'Candidate Pool', profile:'Route Profile'
    }
  };
  const T = key => (tr[state.lang] && tr[state.lang][key]) || tr.en[key] || key;
  const h = s => String(s ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#039;'}[c]));
  const q = (sel, root=document) => root.querySelector(sel);
  const qa = (sel, root=document) => [...root.querySelectorAll(sel)];

  function cpToast(message, bad=false) {
    let stack = q('#cpToastStack');
    if (!stack) {
      stack = document.createElement('div');
      stack.id = 'cpToastStack';
      stack.className = 'cp-toast-stack';
      document.body.appendChild(stack);
    }
    const el = document.createElement('div');
    el.className = 'cp-toast' + (bad ? ' bad' : '');
    el.textContent = message;
    stack.appendChild(el);
    setTimeout(() => el.remove(), 3600);
  }

  function applyTheme() {
    document.documentElement.dataset.theme = state.theme;
    localStorage.setItem('nexaroute_theme', state.theme);
    const b = q('#cpThemeBtn');
    if (b) {
      b.textContent = state.theme === 'dark' ? '☀' : '☾';
      b.title = state.theme === 'dark' ? T('themeLight') : T('themeDark');
    }
  }

  function translateProviderFields(){
    const fa=state.lang==='fa';
    const label=(id,en,faText)=>{const el=q(id);const field=el?.closest('.field');const span=field?.querySelector(':scope > span');if(span)span.textContent=fa?faText:en;};
    label('#pPreset','Provider preset','قالب ارائه‌دهنده');
    label('#pName','Name','نام');
    label('#pId','Internal ID','شناسه داخلی');
    label('#pProtocolMode','Protocol','پروتکل');
    label('#pType','Endpoint type','نوع پروتکل');
    label('#pBase','Base URL','آدرس پایه');
    label('#pAuth','Authentication','احراز هویت');
    label('#pEnabled','Enabled','فعال');
    label('#pKey','API Key','کلید API');
    label('#pKeyEnv','API key environment variable','متغیر محیطی کلید API');
    label('#pHeaders','Extra headers JSON','هدرهای اضافی JSON');
    label('#pProxy','Proxy URL','آدرس Proxy');
    label('#pConcurrency','Max concurrency','حداکثر هم‌زمانی');
    label('#pStreamIdle','Stream idle timeout (seconds)','مهلت بی‌فعالیتی استریم (ثانیه)');
    label('#pModelsPath','Models path','مسیر مدل‌ها');
    label('#pForwardHeaders','Forward request headers','هدرهای قابل انتقال');
    label('#pCredentials','Credential pool JSON','مجموعه کلیدها (JSON)');
    label('#pAliases','Defaults for newly added models','پیش‌فرض مدل‌های تازه');
    const kickers=qa('#providerModal .section-kicker');
    const names=fa?['اتصال','اطلاعات ورود','تنظیمات پیشرفته ارائه‌دهنده','مدل‌ها','بررسی']:['Connection','Credentials','Advanced provider controls','Models','Verification'];
    kickers.forEach((x,i)=>{if(names[i])x.textContent=names[i];});
    if(q('#addModelBtn'))q('#addModelBtn').textContent=fa?'افزودن مدل':'Add model';
    if(q('#checkConnectionBtn'))q('#checkConnectionBtn').textContent=T('testConnection');
    if(q('#testProviderBtn'))q('#testProviderBtn').textContent=T('testModels');
    if(q('#cancelProviderBtn'))q('#cancelProviderBtn').textContent=T('cancel');
    if(q('#deleteProviderBtn'))q('#deleteProviderBtn').textContent=T('delete');
    const title=q('#providerFormTitle');
    if(title) title.textContent=editor?.mode==='edit'?(fa?'ویرایش ارائه‌دهنده':'Edit provider'):(fa?'افزودن ارائه‌دهنده':'Add provider');
  }

  function applyLanguage() {
    document.documentElement.lang = state.lang === 'fa' ? 'fa' : 'en';
    document.documentElement.dir = state.lang === 'fa' ? 'rtl' : 'ltr';
    localStorage.setItem('nexaroute_lang', state.lang);
    const map = {
      overview:'overview', providers:'providers', routing:'routing', models:'models',
      console:'observability', health:'health', cli:'connect', settings:'settings'
    };
    for (const [tab,key] of Object.entries(map)) {
      const b = q(`nav button[data-tab="${tab}"]`);
      if (!b) continue;
      const ico = b.querySelector('.nav-ico')?.outerHTML || '';
      const dot = b.querySelector('.nav-dot')?.outerHTML || '';
      b.innerHTML = ico + h(T(key)) + dot;
    }
    const titleKeys={overview:'overview',console:'observability',providers:'providers',models:'models',health:'health',cli:'connect',settings:'settings',routing:'routing'};
    for(const [tab,key] of Object.entries(titleKeys)){const b=q(`nav button[data-tab="${tab}"]`);if(b)b.dataset.title=T(key);}
    if(typeof subtitles!=='undefined'){
      if(state.lang==='fa'){
        subtitles.overview='سلامت مسیرها، ظرفیت، Failover و بازیابی در یک نمای ساده.';
        subtitles.console='رویدادهای مسیریابی، خطا، Probe و بازیابی به‌صورت زنده.';
        subtitles.providers='APIها، کلیدها و مدل‌های upstream را مدیریت کنید.';
        subtitles.models='وضعیت هر deployment، تأخیر و سلامت مدل‌ها.';
        subtitles.health='فشار Providerها، مدارهای سلامت و بازیابی.';
        subtitles.cli='تنظیم آماده برای Claude Code و کلاینت‌های سازگار.';
        subtitles.settings='تنظیمات موتور مسیریابی، Probe و امنیت.';
      }else{
        subtitles.overview='Health, capacity, failover and recovery in one operational view.';
        subtitles.console='Routing, failure, probe and recovery events as they happen.';
        subtitles.providers='Manage upstream APIs, credentials and model catalogs.';
        subtitles.models='Deployment health, latency and routing state.';
        subtitles.health='Provider pressure, health circuits and recovery.';
        subtitles.cli='Ready-to-copy setup for Claude Code and compatible clients.';
        subtitles.settings='Routing engine, probe and security controls.';
      }
    }
    const lb=q('#cpLangBtn'); if(lb) lb.textContent=T('language');
    const providerTitle=q('#providers .panelhead h2'); if(providerTitle) providerTitle.textContent=T('providers');
    const add=q('#addProviderBtn'); if(add) add.textContent='+ ' + T('addProvider');
    const routeAdd=q('#cpAddRoute'); if(routeAdd) routeAdd.textContent='+ ' + T('addRoute');
    renderRoutingStudio();
    renderOnboarding();
    updateProviderWizardText();
    translateProviderFields();
    const active=q('nav button.active');
    if(active && q('#title')) q('#title').textContent=active.dataset.title||q('#title').textContent;
    if(active && typeof subtitles!=='undefined' && q('#subtitle')) q('#subtitle').textContent=subtitles[active.dataset.tab]||q('#subtitle').textContent;
  }

  function installTopbarTools() {
    const actions = q('.topbar-actions');
    if (!actions || q('#cpLangBtn')) return;
    const tools=document.createElement('div');
    tools.className='cp-toolbar';
    tools.innerHTML=`
      <button id="cpLangBtn" class="cp-icon-btn cp-language" type="button">${h(T('language'))}</button>
      <button id="cpThemeBtn" class="cp-icon-btn" type="button" aria-label="Theme">☀</button>`;
    actions.prepend(tools);
    q('#cpLangBtn').onclick=()=>{ state.lang=state.lang==='en'?'fa':'en'; applyLanguage(); };
    q('#cpThemeBtn').onclick=()=>{ state.theme=state.theme==='dark'?'light':'dark'; applyTheme(); };
  }

  function activateTab(tab,title,subtitle='') {
    qa('nav button').forEach(x=>x.classList.toggle('active',x.dataset.tab===tab));
    qa('.tab').forEach(x=>x.classList.toggle('active',x.id===tab));
    if(q('#title')) q('#title').textContent=title;
    if(q('#subtitle')) q('#subtitle').textContent=subtitle;
    if(tab==='settings' && typeof fillRuntimeSettings==='function') fillRuntimeSettings();
    if(tab==='cli' && typeof renderCLI==='function') renderCLI();
    if(tab==='console' && typeof renderConsole==='function') renderConsole();
    if(tab==='routing') renderRoutingStudio();
  }

  function simplifyNavigation() {
    const nav=q('#nav'); if(!nav || q('nav button[data-tab="routing"]')) return;
    for(const tab of ['virtual','profiles','pools','compat']) q(`nav button[data-tab="${tab}"]`)?.classList.add('cp-hidden-nav');
    const modelsBtn=q('nav button[data-tab="models"]');
    const routeBtn=document.createElement('button');
    routeBtn.dataset.tab='routing'; routeBtn.dataset.title='Routing';
    routeBtn.innerHTML='<span class="nav-ico">⇄</span>'+h(T('routing'));
    nav.insertBefore(routeBtn,modelsBtn);
    routeBtn.onclick=()=>activateTab('routing',T('routing'),T('simpleRouting'));
    const consoleBtn=q('nav button[data-tab="console"]');
    if(consoleBtn) consoleBtn.dataset.title=T('observability');
    const cliBtn=q('nav button[data-tab="cli"]');
    if(cliBtn) cliBtn.dataset.title=T('connect');
  }

  function installRoutingSection() {
    if(q('#routing')) return;
    const section=document.createElement('section');
    section.id='routing'; section.className='tab';
    section.innerHTML=`
      <div class="cp-route-layout">
        <div class="panel">
          <div class="panelhead">
            <div><h2>${h(T('routing'))}</h2><p>${h(T('simpleRouting'))}: public model → backend routing primitives → healthy eligible deployment.</p></div>
            <button id="cpAddRoute" class="btn primary">+ ${h(T('addRoute'))}</button>
          </div>
          <div id="cpRouteList" class="cp-route-list"></div>
        </div>
        <aside class="cp-route-side">
          <div class="cp-help-card">
            <h3>${h(T('advancedRouting'))}</h3>
            <p>${h(T('advancedRoutingCopy'))}</p>
            <div class="cp-advanced-links">
              <button class="btn secondary" data-open-advanced="pools">${h(T('candidatePools'))}</button>
              <button class="btn secondary" data-open-advanced="profiles">${h(T('routeProfiles'))}</button>
              <button class="btn secondary" data-open-advanced="virtual">${h(T('endpoints'))}</button>
              <button class="btn secondary" data-open-advanced="compat">Compatibility</button>
            </div>
          </div>
          <div class="cp-help-card">
            <h3>Client identity stays stable</h3>
            <p>Claude Code can keep using one public model name while NexaRoute changes upstream deployments, absorbs provider failures and supervises recovery.</p>
          </div>
        </aside>
      </div>`;
    q('#providers')?.after(section);
    q('#cpAddRoute').onclick=()=>openRouteDialog();
    qa('[data-open-advanced]',section).forEach(b=>b.onclick=()=>{
      const tab=b.dataset.openAdvanced;
      activateTab(tab, b.textContent, 'Advanced backend routing view');
    });
  }

  function routeParts(ve) {
    const profiles=snap.route_profiles||[], pools=snap.candidate_pools||[], chains=snap.fallback_chains||[];
    const profile=profiles.find(x=>x.id===ve.route_profile);
    const pool=profile ? pools.find(x=>x.id===profile.candidate_pool) : null;
    const chain=profile?.fallback_chain ? chains.find(x=>x.id===profile.fallback_chain) : null;
    return {profile,pool,chain};
  }

  function renderRoutingStudio() {
    const list=q('#cpRouteList'); if(!list) return;
    const routes=snap.virtual_endpoints||[];
    if(!routes.length){
      list.innerHTML=`<div class="cp-empty"><strong>${h(T('noRoutes'))}</strong><span>${h(T('noRoutesCopy'))}</span><button class="btn primary" id="cpEmptyRoute">+ ${h(T('addRoute'))}</button></div>`;
      q('#cpEmptyRoute')?.addEventListener('click',()=>openRouteDialog());
      return;
    }
    list.innerHTML=routes.map(ve=>{
      const {profile,pool,chain}=routeParts(ve);
      const members=pool?.mode==='all' ? T('allEligible') : `${pool?.deployments?.length||ve.pool_member_count||0} deployments`;
      const mode=chain ? T('ordered') : T('automatic');
      return `<article class="cp-route-card">
        <div class="cp-route-head">
          <div class="cp-route-icon">⇄</div>
          <div class="cp-route-copy"><h3>${h(ve.name||ve.public_model||ve.id)}</h3><p><code>${h(ve.public_model||ve.id)}</code> · ${h(mode)}</p></div>
          <span class="pill ${ve.enabled!==false?'on':''}">${ve.enabled!==false?'Enabled':'Disabled'}</span>
        </div>
        <div class="cp-route-meta"><span>${h(members)}</span><span>Profile: ${h(ve.route_profile||'—')}</span>${chain?`<span>${h(chain.pools?.length||0)} fallback stages</span>`:''}</div>
        <div class="cp-route-actions">
          <button class="btn secondary" data-route-edit="${h(ve.id)}">${h(T('edit'))}</button>
          <button class="btn danger-ghost" data-route-delete="${h(ve.id)}">${h(T('delete'))}</button>
        </div>
      </article>`;
    }).join('');
    qa('[data-route-edit]',list).forEach(b=>b.onclick=()=>openRouteDialog(b.dataset.routeEdit));
    qa('[data-route-delete]',list).forEach(b=>b.onclick=()=>deleteSimpleRoute(b.dataset.routeDelete));
  }

  function deploymentListHTML(selected=new Set()) {
    const ds=snap.deployments||[];
    if(!ds.length) return `<div class="cp-model-empty">${h(T('noProvidersCopy'))}</div>`;
    return ds.map(d=>`<label class="cp-check-item">
      <input type="checkbox" value="${h(d.id)}" ${selected.has(d.id)||selected.has(d.model)?'checked':''}>
      <span class="cp-check-copy"><strong>${h(d.model)}</strong><small>${h(d.provider_name||d.provider_id||'')} · ${h(d.id)}</small></span>
    </label>`).join('');
  }

  function openRouteDialog(id='') {
    const ve=(snap.virtual_endpoints||[]).find(x=>x.id===id);
    const parts=ve?routeParts(ve):{};
    let selected=new Set();
    if(parts.pool?.mode==='all') (snap.deployments||[]).forEach(d=>selected.add(d.id));
    else (parts.pool?.deployments||[]).forEach(x=>selected.add(x));
    const mode=parts.chain?'ordered':'automatic';
    openDialog({
      title:ve?T('edit')+' '+T('routing'):T('addRoute'),
      subtitle:'Simple route builder — backend remains authoritative.',
      large:true,
      body:`
        <div class="cp-dialog-error" id="cpRouteError"></div>
        <div class="cp-form-grid">
          <label class="cp-field"><span>${h(T('routeName'))}</span><input id="cpRouteName" value="${h(ve?.name||'Coding')}"></label>
          <label class="cp-field"><span>${h(T('publicModel'))}</span><input id="cpPublicModel" spellcheck="false" value="${h(ve?.public_model||'coding')}"></label>
          <label class="cp-field full"><span>${h(T('routingMode'))}</span>
            <select id="cpRouteMode"><option value="automatic" ${mode==='automatic'?'selected':''}>${h(T('automatic'))}</option><option value="ordered" ${mode==='ordered'?'selected':''}>${h(T('ordered'))}</option></select>
            <small id="cpRouteModeHelp" class="cp-auto-note"></small>
          </label>
          <div class="cp-field full"><span>${h(T('chooseModels'))}</span>
            <div class="cp-model-toolbar"><input id="cpRouteSearch" class="search" type="search" placeholder="${h(T('searchModels'))}">
              <div class="cp-model-actions"><button type="button" class="btn secondary" id="cpRouteAll">${h(T('selectAll'))}</button><button type="button" class="btn secondary" id="cpRouteClear">${h(T('clearAll'))}</button></div>
            </div>
            <div class="cp-check-list" id="cpRouteModels">${deploymentListHTML(selected)}</div>
          </div>
        </div>`,
      actions:[
        {label:T('cancel'),kind:'secondary',value:'cancel'},
        {label:T('saveRoute'),kind:'primary',value:'save'}
      ],
      onReady:host=>{
        const updateHelp=()=>q('#cpRouteModeHelp',host).textContent=q('#cpRouteMode',host).value==='ordered'?T('orderedHelp'):T('automaticHelp');
        q('#cpRouteMode',host).onchange=updateHelp; updateHelp();
        q('#cpRouteAll',host).onclick=()=>qa('#cpRouteModels input',host).forEach(x=>{if(x.closest('.cp-check-item').style.display!=='none')x.checked=true;});
        q('#cpRouteClear',host).onclick=()=>qa('#cpRouteModels input',host).forEach(x=>x.checked=false);
        q('#cpRouteSearch',host).oninput=e=>{
          const s=e.target.value.toLowerCase();
          qa('.cp-check-item',q('#cpRouteModels',host)).forEach(row=>row.style.display=!s||row.textContent.toLowerCase().includes(s)?'flex':'none');
        };
      },
      onAction:async(value,host)=>{
        if(value!=='save') return true;
        const name=q('#cpRouteName',host).value.trim()||'Route';
        const publicModel=q('#cpPublicModel',host).value.trim();
        const mode=q('#cpRouteMode',host).value;
        const deployments=qa('#cpRouteModels input:checked',host).map(x=>x.value);
        const err=q('#cpRouteError',host);
        if(!publicModel){err.textContent='Public model is required.';err.classList.add('show');return false;}
        if(!deployments.length){err.textContent='Select at least one deployment.';err.classList.add('show');return false;}
        try{
          const saveBtn=q('[data-dialog-value="save"]',host); if(saveBtn){saveBtn.disabled=true;saveBtn.textContent=T('saving');}
          await saveSimpleRoute({existing:ve,name,publicModel,mode,deployments,parts});
          return true;
        }catch(e){
          err.textContent=e.message;err.classList.add('show');
          const saveBtn=q('[data-dialog-value="save"]',host); if(saveBtn){saveBtn.disabled=false;saveBtn.textContent=T('saveRoute');}
          return false;
        }
      }
    });
  }

  function slugify(v){ return String(v||'route').toLowerCase().trim().replace(/[^a-z0-9._-]+/g,'-').replace(/^-+|-+$/g,'')||'route'; }
  async function upsert(collection,path,id,body) {
    const exists=(collection||[]).some(x=>x.id===id);
    return api(path+(exists?'/'+encodeURIComponent(id):''),{
      method:exists?'PUT':'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)
    });
  }

  async function saveSimpleRoute({existing,name,publicModel,mode,deployments,parts}) {
    const base=slugify(existing?.public_model||publicModel);
    let primaryPool=parts?.profile?.candidate_pool || `route-${base}-pool`;
    let fallbackChain='';
    if(mode==='ordered'){
      const poolIDs=[];
      for(let i=0;i<deployments.length;i++){
        const pid=`route-${base}-stage-${i+1}`;
        await upsert(snap.candidate_pools,'/admin/api/candidate-pools',pid,{id:pid,name:`${name} — stage ${i+1}`,mode:'explicit',deployments:[deployments[i]]});
        poolIDs.push(pid);
      }
      primaryPool=poolIDs[0];
      fallbackChain=parts?.profile?.fallback_chain || `route-${base}-fallback`;
      await upsert(snap.fallback_chains,'/admin/api/fallback-chains',fallbackChain,{id:fallbackChain,name:`${name} fallback`,pools:poolIDs});
    }else{
      await upsert(snap.candidate_pools,'/admin/api/candidate-pools',primaryPool,{id:primaryPool,name:`${name} models`,mode:'explicit',deployments});
    }
    const profileID=existing?.route_profile || `route-${base}-profile`;
    await upsert(snap.route_profiles,'/admin/api/route-profiles',profileID,{
      id:profileID,name:`${name} profile`,candidate_pool:primaryPool,...(fallbackChain?{fallback_chain:fallbackChain}:{})
    });
    const veID=existing?.id || `route-${base}`;
    const body={id:veID,name,public_model:publicModel,route_profile:profileID,enabled:existing?.enabled!==false};
    await upsert(snap.virtual_endpoints,'/admin/api/virtual-endpoints',veID,body);
    await refresh();
    cpToast(T('saved'));
  }

  async function deleteSimpleRoute(id) {
    const ve=(snap.virtual_endpoints||[]).find(x=>x.id===id); if(!ve)return;
    const ok=await UI.confirm(T('deleteRoute'),`${T('destructive')}\n${ve.public_model||ve.id}`);
    if(!ok)return;
    const {profile,pool,chain}=routeParts(ve);
    const prefix='route-'+slugify(ve.public_model||ve.id);
    try{
      await api('/admin/api/virtual-endpoints/'+encodeURIComponent(id),{method:'DELETE'});
      const profileShared=(snap.virtual_endpoints||[]).some(x=>x.id!==id&&x.route_profile===profile?.id);
      if(profile && !profileShared && profile.id.startsWith(prefix)){
        await api('/admin/api/route-profiles/'+encodeURIComponent(profile.id),{method:'DELETE'}).catch(()=>{});
        const chainShared=(snap.route_profiles||[]).some(x=>x.id!==profile.id&&x.fallback_chain===chain?.id);
        if(chain && !chainShared && chain.id.startsWith(prefix)) await api('/admin/api/fallback-chains/'+encodeURIComponent(chain.id),{method:'DELETE'}).catch(()=>{});
        const candidates=(snap.candidate_pools||[]).filter(p=>p.id.startsWith(prefix));
        for(const p of candidates){
          const usedByOther=(snap.route_profiles||[]).some(x=>x.id!==profile.id&&x.candidate_pool===p.id) ||
            (snap.fallback_chains||[]).some(x=>x.id!==chain?.id&&(x.pools||[]).includes(p.id));
          if(!usedByOther) await api('/admin/api/candidate-pools/'+encodeURIComponent(p.id),{method:'DELETE'}).catch(()=>{});
        }
      }
      await refresh();cpToast(T('saved'));
    }catch(e){cpToast(e.message,true);}
  }

  function renderOnboarding() {
    let hero=q('#cpOnboarding');
    const overview=q('#overview'); if(!overview)return;
    if(!hero){
      hero=document.createElement('div');hero.id='cpOnboarding';hero.className='cp-hero cp-onboarding-banner';
      overview.prepend(hero);
    }
    const providers=providerSummaries||[], routes=snap.virtual_endpoints||[];
    if(providers.length && routes.length){hero.classList.remove('show');return;}
    hero.classList.add('show');
    hero.innerHTML=`
      <div><h2>${h(providers.length?T('createRoute'):T('welcome'))}</h2><p>${h(providers.length?T('noRoutesCopy'):T('welcomeCopy'))}</p>
      <div class="cp-hero-actions">${!providers.length?`<button class="btn primary" id="cpHeroProvider">${h(T('connectProvider'))}</button>`:`<button class="btn primary" id="cpHeroRoute">${h(T('createRoute'))}</button>`}</div></div>
      <div class="cp-hero-checks">
        <div class="cp-hero-check"><i>${providers.length?'✓':'1'}</i><span>${h(T('providers'))}: ${providers.length}</span></div>
        <div class="cp-hero-check"><i>${(snap.deployments||[]).length?'✓':'2'}</i><span>${h(T('models'))}: ${(snap.deployments||[]).length}</span></div>
        <div class="cp-hero-check"><i>${routes.length?'✓':'3'}</i><span>${h(T('routing'))}: ${routes.length}</span></div>
      </div>`;
    q('#cpHeroProvider')?.addEventListener('click',()=>q('#addProviderBtn')?.click());
    q('#cpHeroRoute')?.addEventListener('click',()=>{activateTab('routing',T('routing'));openRouteDialog();});
  }

  function installProviderSearch() {
    const head=q('#providers .panelhead'); if(!head || q('#cpProviderSearch'))return;
    const oldBtn=q('#addProviderBtn');
    const actions=document.createElement('div');actions.className='cp-page-actions';
    const search=document.createElement('input');search.id='cpProviderSearch';search.className='search cp-provider-search';search.type='search';search.placeholder=T('searchProviders');
    search.oninput=()=>{state.providerSearch=search.value.toLowerCase();filterProviderCards();};
    oldBtn?.before(actions);actions.append(search);if(oldBtn)actions.append(oldBtn);
    const observer=new MutationObserver(()=>filterProviderCards());observer.observe(q('#providerGrid'),{childList:true});
  }
  function filterProviderCards(){
    const s=state.providerSearch;
    qa('#providerGrid .provider-card').forEach(c=>c.style.display=!s||c.textContent.toLowerCase().includes(s)?'block':'none');
  }

  function nextProviderIdentity(){
    const used=new Set((providerSummaries||[]).map(p=>p.id));
    let n=1;while(used.has(`provider-${n}`))n++;
    return {name:`Provider ${n}`,id:`provider-${n}`};
  }

  function installProviderWizard() {
    const drawer=q('#providerModal .drawer'); if(!drawer || q('#cpProviderProgress'))return;
    const progress=document.createElement('div');progress.id='cpProviderProgress';progress.className='cp-provider-progress';
    progress.innerHTML=[['connection',1],['modelsStep',2],['verify',3]].map(([k,n])=>`<button type="button" class="cp-step" data-provider-step="${n-1}"><span>${n}</span><b>${h(T(k))}</b></button>`).join('');
    q('.drawer-head',drawer).after(progress);
    const sections=qa('.drawer-scroll > .form-section',drawer);
    sections.forEach((s,i)=>{s.classList.add('cp-wizard-section');s.dataset.providerSection=String(i);});
    if(sections[2]){
      const details=document.createElement('details');details.className='cp-advanced-toggle';details.dataset.providerSection='2';
      details.innerHTML=`<summary>${h(T('advanced'))}</summary>`;
      sections[2].before(details);details.append(sections[2]);
    }
    const idField=q('#pId')?.closest('.field');
    if(idField && sections[2]) sections[2].prepend(idField);
    const typeField=q('#pType')?.closest('.field');
    if(typeField && !q('#pProtocolMode')){
      const proto=document.createElement('label');proto.className='field';proto.innerHTML=`<span>${h(T('protocol'))}</span><select id="pProtocolMode"><option value="auto">${h(T('protocolAuto'))}</option><option value="manual">${h(T('protocolManual'))}</option></select>`;
      typeField.before(proto);
      q('#pProtocolMode').onchange=e=>{state.autoProtocol=e.target.value==='auto';syncProtocolMode();};
    }
    const back=document.createElement('button');back.id='cpProviderBack';back.className='btn secondary';back.type='button';back.textContent=T('back');
    const next=document.createElement('button');next.id='cpProviderNext';next.className='btn primary';next.type='button';next.textContent=T('next');
    q('#cancelProviderBtn').before(back,next);
    back.onclick=()=>setProviderStep(Math.max(0,state.providerStep-1));
    next.onclick=()=>advanceProviderStep();
    qa('[data-provider-step]',progress).forEach(b=>b.onclick=()=>{
      const target=Number(b.dataset.providerStep);
      if(target<=state.providerStep) setProviderStep(target);
      else if(target===state.providerStep+1) advanceProviderStep();
      else cpToast(state.lang==='fa'?'ابتدا مرحله قبلی را تکمیل کنید.':'Complete the previous step first.',true);
    });
    q('#addProviderBtn').onclick=()=>{
      const ident=nextProviderIdentity();
      editor={mode:'add',originalId:'',provider:emptyProvider(),detected:[],selected:new Set(),modelMeta:new Map(),secretDirty:true,secretSource:'none'};
      editor.provider.name=ident.name;editor.provider.id=ident.id;
      fillForm();q('#pId').dataset.autogen='1';state.autoProtocol=true;q('#pProtocolMode').value='auto';syncProtocolMode();setProviderStep(0);modal(true);
    };
    const originalFill=fillForm;
    fillForm=function(){originalFill();syncProviderUX();};
    const originalPicker=renderPicker;
    renderPicker=function(){originalPicker();enhanceModelPicker();};
    q('#discoverBtn').onclick=discoverModelsV2;
    q('#deleteProviderBtn').onclick=async()=>{
      if(!editor?.originalId)return;
      if(!await UI.confirm(T('deleteProvider'),`${T('destructive')}\n${editor.originalId}`))return;
      try{await api('/admin/api/providers/'+encodeURIComponent(editor.originalId),{method:'DELETE'});modal(false);await refresh();cpToast(T('saved'));}catch(e){cpToast(e.message,true);}
    };
    setProviderStep(0);
  }

  function syncProtocolMode(){
    const manual=!state.autoProtocol;
    const type=q('#pType')?.closest('.field');if(type)type.style.display=manual?'grid':'none';
    const auth=q('#pAuth')?.closest('.field');if(auth)auth.style.display=manual?'grid':'none';
  }

  function syncProviderUX(){
    if(!editor)return;
    if(editor.mode==='edit' && !editor._savedSelected) editor._savedSelected=new Set(editor.selected||[]);

    if(editor.mode==='add'){
      q('#pId').dataset.autogen=q('#pId').dataset.autogen||'1';
      q('#pProtocolMode').value='auto';state.autoProtocol=true;
    }else{
      q('#pId').dataset.autogen='0';
      q('#pProtocolMode').value='manual';state.autoProtocol=false;
    }
    syncProtocolMode();
    installSecretSavedState();
    enhanceModelPicker();
    translateProviderFields();
    setProviderStep(0);
  }

  function installSecretSavedState(){
    const key=q('#pKey'); if(!key)return;
    const field=key.closest('.field');
    q('.cp-secret-saved',field)?.remove();
    field.classList.remove('cp-secret-replace-hidden');
    if(editor?.mode==='edit' && editor.secretSource && editor.secretSource!=='none'){
      const box=document.createElement('div');box.className='cp-secret-saved';
      box.innerHTML=`<strong>● ${h(T('savedCredential'))}</strong><span>${h(T('keepSaved'))}</span><button type="button" class="btn secondary">${h(T('replace'))}</button>`;
      field.prepend(box);
      q('.secret-row',field)?.classList.add('cp-secret-replace-hidden');
      const env=q('#pKeyEnv')?.closest('.field'); env?.classList.add('cp-secret-replace-hidden');
      box.querySelector('button').onclick=()=>{
        q('.secret-row',field)?.classList.remove('cp-secret-replace-hidden');
        env?.classList.remove('cp-secret-replace-hidden');
        key.focus();editor.secretDirty=true;box.remove();
      };
    }else{
      q('.secret-row',field)?.classList.remove('cp-secret-replace-hidden');
      q('#pKeyEnv')?.closest('.field')?.classList.remove('cp-secret-replace-hidden');
    }
  }

  function setProviderStep(step){
    state.providerStep=Math.max(0,Math.min(2,step));
    qa('#providerModal .cp-step').forEach((b,i)=>{b.classList.toggle('active',i===state.providerStep);b.classList.toggle('done',i<state.providerStep);});
    const sections=qa('#providerModal .drawer-scroll > .form-section, #providerModal .drawer-scroll > details[data-provider-section]');
    sections.forEach(el=>{
      const raw=Number(el.dataset.providerSection ?? el.querySelector('.form-section')?.dataset.providerSection ?? -1);
      const visible=state.providerStep===0 ? [0,1,2].includes(raw) : state.providerStep===1 ? raw===3 : raw===4;
      el.classList.toggle('cp-step-hidden',!visible);
    });
    q('#cpProviderBack').style.display=state.providerStep?'inline-flex':'none';
    q('#cpProviderNext').style.display=state.providerStep<2?'inline-flex':'none';
    q('#saveProviderBtn').style.display=state.providerStep===2?'inline-flex':'none';
    q('#cancelProviderBtn').style.display='inline-flex';
    q('#providerModal .drawer-scroll').scrollTop=0;
    updateProviderWizardText();
  }

  function updateProviderWizardText(){
    qa('#cpProviderProgress .cp-step b').forEach((b,i)=>b.textContent=[T('connection'),T('modelsStep'),T('verify')][i]);
    if(q('#cpProviderBack'))q('#cpProviderBack').textContent=T('back');
    if(q('#cpProviderNext'))q('#cpProviderNext').textContent=T('next');
    if(q('#saveProviderBtn'))q('#saveProviderBtn').textContent=T('saveProvider');
    if(q('#discoverBtn'))q('#discoverBtn').textContent=T('detectModels');
    const protocol=q('#pProtocolMode');
    if(protocol){
      const auto=protocol.querySelector('option[value="auto"]'),manual=protocol.querySelector('option[value="manual"]');
      if(auto)auto.textContent=T('protocolAuto');if(manual)manual.textContent=T('protocolManual');
    }
  }

  function advanceProviderStep(){
    try{
      if(state.providerStep===0){
        const base=q('#pBase').value.trim(); if(!base)throw new Error('Base URL is required');
        if(!q('#pName').value.trim())q('#pName').value=nextProviderIdentity().name;
        if(!q('#pId').value.trim())q('#pId').value=slugify(q('#pName').value);
      }
      if(state.providerStep===1 && !editor.selected.size)throw new Error('Select at least one model');
      setProviderStep(state.providerStep+1);
    }catch(e){cpToast(e.message,true);}
  }

  function enhanceModelPicker(){
    const picker=q('#modelPicker'); if(!picker)return;
    let toolbar=q('#cpModelToolbar');
    if(!toolbar){
      toolbar=document.createElement('div');toolbar.id='cpModelToolbar';toolbar.className='cp-model-toolbar';
      toolbar.innerHTML=`
        <input id="cpModelSearch" class="search" type="search" placeholder="${h(T('searchModels'))}">
        <div class="cp-model-actions">
          <span id="cpModelCount" class="cp-selection-count"></span>
          <button type="button" id="cpSelectAll" class="btn secondary">${h(T('selectAll'))}</button>
          <button type="button" id="cpSelectVisible" class="btn secondary">${h(T('selectVisible'))}</button>
          <button type="button" id="cpClearAll" class="btn secondary">${h(T('clearAll'))}</button>
          <button type="button" id="cpModelAdvanced" class="btn ghost">${h(T('advancedModels'))}</button>
        </div>`;
      picker.before(toolbar);
      q('#cpModelSearch').oninput=e=>{state.modelSearch=e.target.value.toLowerCase();filterModels();};
      q('#cpSelectAll').onclick=()=>{qa('#modelPicker .model-select').forEach(x=>{x.checked=true;editor.selected.add(x.dataset.model);});updateModelCount();};
      q('#cpSelectVisible').onclick=()=>{qa('#modelPicker .model-option').filter(r=>r.style.display!=='none').forEach(r=>{const x=q('.model-select',r);x.checked=true;editor.selected.add(x.dataset.model);});updateModelCount();};
      q('#cpClearAll').onclick=()=>{qa('#modelPicker .model-select').forEach(x=>x.checked=false);editor.selected.clear();updateModelCount();};
      q('#cpModelAdvanced').onclick=()=>{state.modelAdvanced=!state.modelAdvanced;picker.classList.toggle('cp-show-model-advanced',state.modelAdvanced);};
    }
    qa('.model-option',picker).forEach(row=>{
      const input=q('.model-select',row); if(!input)return;
      const meta=editor.modelMeta?.get(input.dataset.model)||{};
      const head=q('.model-option-head',row);
      if(!q('.cp-model-badges',head)){
        const badges=document.createElement('div');badges.className='cp-model-badges';
        const caps=meta.capabilities||{};
        const vals=[];if(caps.tools!==false)vals.push('Tools');if(caps.vision)vals.push('Vision');if(caps.reasoning)vals.push('Reason');if(caps.streaming!==false)vals.push('Stream');
        const saved=editor?._savedSelected?.has(input.dataset.model);
        const discovered=editor?._lastDiscovered?.has(input.dataset.model);
        if(editor?.mode==='edit' && editor._lastDiscovered){
          if(saved && !discovered) vals.unshift('<saved>');
          else if(!saved && discovered) vals.unshift('<new>');
        }
        badges.innerHTML=vals.map(v=>v==='<saved>'?'<span class="cp-model-badge cp-stale-model">Saved · not discovered</span>':v==='<new>'?'<span class="cp-model-badge cp-new-model">New</span>':`<span class="cp-model-badge">${v}</span>`).join('');
        head.appendChild(badges);
      }
      const old=input.onchange;
      input.onchange=()=>{old?.();updateModelCount();};
    });
    picker.classList.toggle('cp-show-model-advanced',state.modelAdvanced);
    filterModels();updateModelCount();
  }

  function filterModels(){
    const s=state.modelSearch;
    qa('#modelPicker .model-option').forEach(row=>row.style.display=!s||row.textContent.toLowerCase().includes(s)?'block':'none');
  }
  function updateModelCount(){
    const el=q('#cpModelCount');if(el)el.textContent=`${editor?.selected?.size||0} ${T('selected')}`;
  }

  async function discoverModelsV2(){
    try{
      let p=readForm();
      const button=q('#discoverBtn');button.disabled=true;q('#discoverStatus').textContent=T('detecting');
      const candidates=state.autoProtocol
        ? [
            ['openai_compatible','bearer'],['anthropic_compatible','x-api-key'],['openai_responses','bearer'],['gemini','x-goog-api-key']
          ]
        : [[p.type,p.auth_mode]];
      let best=null,lastErr='';
      for(const [type,auth] of candidates){
        const trial={...p,type,auth_mode:p.auth_mode==='none'?'none':auth};
        try{
          const d=await api('/admin/api/provider-discover',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(payload(trial))});
          if(d.ok && (d.models||[]).length){best={d,type,auth};break;}
        }catch(e){lastErr=e.message;}
      }
      if(!best)throw new Error(lastErr||'No models discovered');
      q('#pType').value=best.type;
      if(q('#pAuth').value!=='none')q('#pAuth').value=best.auth;
      editor._lastDiscovered=new Set(best.d.models||[]);
      editor.detected=[...new Set([...(best.d.models||[]),...editor.detected])];
      (best.d.models||[]).forEach((m,i)=>ensureModelMeta(m,i));
      renderPicker();
      q('#discoverStatus').textContent=`${best.d.models.length} ${T('found')} · ${best.type}`;
    }catch(e){q('#discoverStatus').textContent=e.message;cpToast(e.message,true);}
    finally{q('#discoverBtn').disabled=false;}
  }

  function openDialog({title,subtitle='',body='',large=false,actions=[],onReady,onAction}) {
    let host=q('#cpDialogHost');
    if(!host){host=document.createElement('div');host.id='cpDialogHost';host.className='cp-dialog-host';document.body.appendChild(host);}
    host.innerHTML=`<div class="cp-dialog-backdrop"></div><div class="cp-dialog ${large?'cp-dialog-lg':''}" role="dialog" aria-modal="true">
      <div class="cp-dialog-head"><div><h2>${h(title)}</h2><p>${h(subtitle)}</p></div><button class="close-btn" data-dialog-close aria-label="Close">×</button></div>
      <div class="cp-dialog-body">${body}</div>
      <div class="cp-dialog-foot"><div class="spacer"></div>${actions.map(a=>`<button class="btn ${a.kind||'secondary'}" data-dialog-value="${h(a.value)}">${h(a.label)}</button>`).join('')}</div>
    </div>`;
    host.classList.add('open');
    const close=()=>{host.classList.remove('open');host.innerHTML='';};
    q('[data-dialog-close]',host).onclick=close;q('.cp-dialog-backdrop',host).onclick=close;
    qa('[data-dialog-value]',host).forEach(b=>b.onclick=async()=>{
      const value=b.dataset.dialogValue;
      if(value==='cancel'){close();return;}
      const shouldClose=onAction ? await onAction(value,host) : true;
      if(shouldClose!==false)close();
    });
    onReady?.(host);
    return {host,close};
  }
  UI.openDialog=openDialog;

  UI.confirm=(title,message)=>new Promise(resolve=>{
    const {close}=openDialog({
      title,body:`<div class="cp-confirm-icon">!</div><div class="cp-confirm-copy">${h(message).replace(/\n/g,'<br>')}</div>`,
      actions:[{label:T('cancel'),kind:'secondary',value:'no'},{label:T('delete'),kind:'danger-ghost',value:'yes'}],
      onAction:value=>{resolve(value==='yes');return true;}
    });
    q('[data-dialog-close]',q('#cpDialogHost')).onclick=()=>{resolve(false);close();};
    q('.cp-dialog-backdrop',q('#cpDialogHost')).onclick=()=>{resolve(false);close();};
  });

  UI.requestAdminKey=()=>new Promise(resolve=>{
    const {close}=openDialog({
      title:T('adminKey'),subtitle:T('adminKeyCopy'),
      body:`<label class="cp-field"><span>x-admin-key</span><input id="cpAdminKeyInput" type="password" autocomplete="off"></label>`,
      actions:[{label:T('cancel'),kind:'secondary',value:'dismiss'},{label:T('save'),kind:'primary',value:'save'}],
      onReady:host=>{q('#cpAdminKeyInput',host).focus();q('#cpAdminKeyInput',host).onkeydown=e=>{if(e.key==='Enter')q('[data-dialog-value="save"]',host).click();};},
      onAction:(v,host)=>{if(v!=='save'){resolve(null);return true;}const key=q('#cpAdminKeyInput',host).value.trim();resolve(key||null);return true;}
    });
    q('[data-dialog-close]',q('#cpDialogHost')).onclick=()=>{resolve(null);close();};
  });

  function installAdminKeyFlow(){
    apiFetch=async function(url,opt={}){
      opt={...opt,headers:{...(opt.headers||{})}};
      if(adminKey)opt.headers['x-admin-key']=adminKey;
      let r=await window.fetch(url,opt);
      if(r.status===401){
        const key=await UI.requestAdminKey();
        if(key){
          adminKey=key;sessionStorage.setItem('nexaroute_admin_key',adminKey);
          if(q('#adminKey'))q('#adminKey').value=adminKey;
          opt.headers['x-admin-key']=adminKey;r=await window.fetch(url,opt);
        }
      }
      return r;
    };
  }

  async function advancedVirtual(id=''){
    const x=(snap.virtual_endpoints||[]).find(v=>v.id===id);
    const profiles=snap.route_profiles||[];
    formDialog({
      title:x?T('edit')+' '+T('endpoints'):T('addRoute'),
      fields:[
        {id:'id',label:T('id'),value:x?.id||'endpoint-1',disabled:!!x},
        {id:'name',label:T('displayName'),value:x?.name||''},
        {id:'public_model',label:T('publicModel'),value:x?.public_model||'coding'},
        {id:'route_profile',label:T('profile'),type:'select',value:x?.route_profile||'',options:profiles.map(p=>[p.id,p.name||p.id])},
        {id:'enabled',label:T('enabled'),type:'checkbox',value:x?.enabled!==false}
      ],
      save:async v=>{const body={id:x?.id||v.id,name:v.name,public_model:v.public_model,route_profile:v.route_profile,enabled:v.enabled};await upsert(snap.virtual_endpoints,'/admin/api/virtual-endpoints',body.id,body);await refresh();}
    });
  }
  async function advancedProfile(id=''){
    const x=(snap.route_profiles||[]).find(v=>v.id===id), pools=snap.candidate_pools||[],chains=snap.fallback_chains||[];
    formDialog({title:T('routeProfiles'),fields:[
      {id:'id',label:T('id'),value:x?.id||'profile-1',disabled:!!x},{id:'name',label:T('displayName'),value:x?.name||''},
      {id:'candidate_pool',label:T('pool'),type:'select',value:x?.candidate_pool||'',options:pools.map(p=>[p.id,p.name||p.id])},
      {id:'fallback_chain',label:T('fallback'),type:'select',value:x?.fallback_chain||'',options:[['','—'],...chains.map(c=>[c.id,c.name||c.id])]}
    ],save:async v=>{const body={id:x?.id||v.id,name:v.name,candidate_pool:v.candidate_pool,...(v.fallback_chain?{fallback_chain:v.fallback_chain}:{})};await upsert(snap.route_profiles,'/admin/api/route-profiles',body.id,body);await refresh();}});
  }
  async function advancedPool(id=''){
    const x=(snap.candidate_pools||[]).find(v=>v.id===id);
    formDialog({title:T('candidatePools'),large:true,fields:[
      {id:'id',label:T('id'),value:x?.id||'pool-1',disabled:!!x},{id:'name',label:T('displayName'),value:x?.name||''},
      {id:'mode',label:T('mode'),type:'select',value:x?.mode||'explicit',options:[['explicit',T('explicit')],['all',T('allEligible')]]},
      {id:'deployments',label:T('chooseModels'),type:'checks',value:new Set(x?.deployments||[]),options:(snap.deployments||[]).map(d=>[d.id,`${d.model} — ${d.provider_name||d.provider_id}`])}
    ],save:async v=>{const body={id:x?.id||v.id,name:v.name,mode:v.mode,deployments:v.mode==='all'?[]:v.deployments};await upsert(snap.candidate_pools,'/admin/api/candidate-pools',body.id,body);await refresh();}});
  }
  async function advancedChain(id=''){
    const x=(snap.fallback_chains||[]).find(v=>v.id===id),pools=snap.candidate_pools||[];
    formDialog({title:T('fallbackChains'),fields:[
      {id:'id',label:T('id'),value:x?.id||'fallback-1',disabled:!!x},{id:'name',label:T('displayName'),value:x?.name||''},
      {id:'pools',label:T('pools'),value:(x?.pools||[]).join(', ')}
    ],save:async v=>{const ps=v.pools.split(',').map(s=>s.trim()).filter(Boolean);for(const p of ps)if(!pools.some(x=>x.id===p))throw new Error('Unknown pool: '+p);if(!ps.length)throw new Error('At least one pool required');const body={id:x?.id||v.id,name:v.name,pools:ps};await upsert(snap.fallback_chains,'/admin/api/fallback-chains',body.id,body);await refresh();}});
  }

  function formDialog({title,fields,save,large=false}) {
    const body=`<div class="cp-dialog-error" id="cpFormError"></div><div class="cp-form-grid">${fields.map(f=>{
      if(f.type==='select')return `<label class="cp-field"><span>${h(f.label)}</span><select data-form="${h(f.id)}">${(f.options||[]).map(o=>`<option value="${h(o[0])}" ${String(o[0])===String(f.value)?'selected':''}>${h(o[1])}</option>`).join('')}</select></label>`;
      if(f.type==='checkbox')return `<label class="cp-field"><span>${h(f.label)}</span><input data-form="${h(f.id)}" type="checkbox" ${f.value?'checked':''}></label>`;
      if(f.type==='checks')return `<div class="cp-field full"><span>${h(f.label)}</span><div class="cp-check-list" data-checks="${h(f.id)}">${(f.options||[]).map(o=>`<label class="cp-check-item"><input type="checkbox" value="${h(o[0])}" ${f.value?.has(o[0])?'checked':''}><span class="cp-check-copy"><strong>${h(o[1])}</strong><small>${h(o[0])}</small></span></label>`).join('')}</div></div>`;
      return `<label class="cp-field"><span>${h(f.label)}</span><input data-form="${h(f.id)}" value="${h(f.value||'')}" ${f.disabled?'disabled':''}></label>`;
    }).join('')}</div>`;
    openDialog({title,body,large,actions:[{label:T('cancel'),kind:'secondary',value:'cancel'},{label:T('save'),kind:'primary',value:'save'}],onAction:async(v,host)=>{
      if(v!=='save')return true;const vals={};
      fields.forEach(f=>{
        if(f.type==='checks')vals[f.id]=qa(`[data-checks="${f.id}"] input:checked`,host).map(x=>x.value);
        else{const el=q(`[data-form="${f.id}"]`,host);vals[f.id]=f.type==='checkbox'?el.checked:el.value.trim();}
      });
      try{await save(vals);cpToast(T('saved'));return true;}catch(e){const er=q('#cpFormError',host);er.textContent=e.message;er.classList.add('show');return false;}
    }});
  }

  function overrideAdvancedEditors(){
    UI.advancedVirtual=advancedVirtual;
    UI.advancedProfile=advancedProfile;
    UI.advancedPool=advancedPool;
    UI.advancedChain=advancedChain;
    UI.deleteAdvanced=deleteAdvanced;
    window.editVirtual=id=>advancedVirtual(id);
    window.editProfile=id=>advancedProfile(id);
    window.editPool=id=>advancedPool(id);
    window.editChain=id=>advancedChain(id);
    window.deleteVirtual=id=>deleteAdvanced('/admin/api/virtual-endpoints',id,'virtual endpoint');
    window.deleteProfile=id=>deleteAdvanced('/admin/api/route-profiles',id,'route profile');
    window.deletePool=id=>deleteAdvanced('/admin/api/candidate-pools',id,'candidate pool');
    window.deleteChain=id=>deleteAdvanced('/admin/api/fallback-chains',id,'fallback chain');
    const binds=[['#addVirtualBtn',()=>advancedVirtual()],['#addProfileBtn',()=>advancedProfile()],['#addPoolBtn',()=>advancedPool()],['#addChainBtn',()=>advancedChain()]];
    for(const [sel,fn] of binds){const el=q(sel);if(!el)continue;el.addEventListener('click',e=>{e.preventDefault();e.stopImmediatePropagation();fn();},true);}
  }
  async function deleteAdvanced(path,id,label){
    if(!await UI.confirm(T('confirmDelete'),`${T('destructive')}\n${label}: ${id}`))return;
    try{await api(path+'/'+encodeURIComponent(id),{method:'DELETE'});await refresh();cpToast(T('saved'));}catch(e){cpToast(e.message,true);}
  }

  function enhanceProviderCards(){
    const grid=q('#providerGrid');if(!grid)return;
    qa('.provider-card',grid).forEach(card=>{
      if(q('.cp-card-actions',card))return;
      const edit=q('.edit-provider',card);if(!edit)return;
      const actions=document.createElement('div');actions.className='cp-card-actions';
      edit.before(actions);actions.append(edit);
    });
  }

  function wrapRender(){
    const oldRender=render;
    render=function(){
      oldRender();
      renderRoutingStudio();renderOnboarding();enhanceProviderCards();filterProviderCards();
    };
  }

  function patchProviderSaveFlow(){
    const btn=q('#saveProviderBtn'); if(!btn)return;
    const original=btn.onclick;
    btn.onclick=async function(){
      if(original) await original.call(btn);
      if(!q('#providerModal')?.classList.contains('open')) setProviderStep(0);
    };
  }

  function init(){
    document.body.classList.add('cp-v2');
    installTopbarTools();simplifyNavigation();installRoutingSection();installProviderSearch();
    installProviderWizard();installAdminKeyFlow();overrideAdvancedEditors();wrapRender();patchProviderSaveFlow();
    applyTheme();applyLanguage();
    renderRoutingStudio();renderOnboarding();enhanceProviderCards();
  }

  if(document.readyState==='loading')document.addEventListener('DOMContentLoaded',init);else init();
})();
