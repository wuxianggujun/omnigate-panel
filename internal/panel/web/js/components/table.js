// table.js — 高密度数据表格；所有列渲染结果按 DOM 节点或纯文本处理。

import { el, icon } from '../core/dom.js';

/**
 * @param {{
 *  columns: Array<{key:string,label:string,render?:Function,className?:string,align?:string,width?:string,hideOnMobile?:boolean}>,
 *  rows: Array<object>,
 *  rowKey?: (row:object,index:number)=>string,
 *  onRowClick?: (row:object,index:number)=>void,
 *  empty?: string,
 *  loading?: boolean,
 *  caption?: string,
 *  class?: string,
 *  getRowClass?: (row:object)=>string,
 * }} options
 */
export function createTable({
  columns = [],
  rows = [],
  rowKey = null,
  onRowClick = null,
  empty = '暂无数据',
  loading = false,
  caption = '',
  class: className = '',
  getRowClass = null,
} = {}) {
  let currentRows = Array.isArray(rows) ? rows : [];
  let currentLoading = !!loading;

  const thead = el('thead');
  const tbody = el('tbody');
  const table = el('table', { class: `data-table ${className}`.trim() },
    caption ? el('caption', { class: 'visually-hidden', text: caption }) : null,
    thead,
    tbody,
  );
  const wrap = el('div', { class: 'table-wrap' }, table);

  function renderHead() {
    const tr = el('tr');
    for (const col of columns) {
      tr.appendChild(el('th', {
        class: [col.align === 'right' ? 'align-right' : '', col.align === 'center' ? 'align-center' : '', col.hideOnMobile ? 'hide-mobile' : '', col.className || ''].filter(Boolean).join(' '),
        style: col.width ? { width: col.width } : undefined,
        attrs: { scope: 'col' },
      }, col.label));
    }
    thead.replaceChildren(tr);
  }

  function renderBody() {
    if (currentLoading && !currentRows.length) {
      const colCount = Math.max(1, columns.length);
      tbody.replaceChildren(el('tr', { class: 'table-state-row' },
        el('td', { colspan: String(colCount) },
          el('div', { class: 'table-loading' }, el('span', { class: 'spinner', 'aria-hidden': 'true' }), '加载中'),
        ),
      ));
      return;
    }
    if (!currentRows.length) {
      const colCount = Math.max(1, columns.length);
      tbody.replaceChildren(el('tr', { class: 'table-state-row' },
        el('td', { colspan: String(colCount) },
          el('div', { class: 'table-empty' }, icon('info', 16), empty),
        ),
      ));
      return;
    }
    const frag = document.createDocumentFragment();
    currentRows.forEach((row, index) => {
      const tr = el('tr', {
        class: [onRowClick ? 'row-clickable' : '', typeof getRowClass === 'function' ? getRowClass(row) : ''].filter(Boolean).join(' '),
        dataset: { rowIndex: String(index), ...(rowKey ? { rowKey: String(rowKey(row, index)) } : {}) },
        attrs: onRowClick ? { tabindex: '0' } : undefined,
      });
      for (const col of columns) {
        let content;
        try {
          content = typeof col.render === 'function' ? col.render(row, index) : row?.[col.key];
        } catch (error) {
          content = el('span', { class: 'text-danger', text: '渲染失败' });
          console.error('table cell render failed', error);
        }
        const td = el('td', {
          class: [col.align === 'right' ? 'align-right' : '', col.align === 'center' ? 'align-center' : '', col.hideOnMobile ? 'hide-mobile' : '', col.className || ''].filter(Boolean).join(' '),
        });
        if (content == null) {
          td.textContent = '—';
        } else if (content instanceof Node) {
          td.appendChild(content);
        } else {
          td.textContent = String(content);
        }
        tr.appendChild(td);
      }
      frag.appendChild(tr);
    });
    tbody.replaceChildren(frag);
  }

  tbody.addEventListener('click', (event) => {
    if (!onRowClick) return;
    const tr = event.target instanceof Element ? event.target.closest('tr[data-row-index]') : null;
    if (!tr || !tbody.contains(tr)) return;
    const index = Number(tr.dataset.rowIndex);
    if (Number.isFinite(index) && currentRows[index]) onRowClick(currentRows[index], index);
  });
  tbody.addEventListener('keydown', (event) => {
    if (!onRowClick || (event.key !== 'Enter' && event.key !== ' ')) return;
    const tr = event.target instanceof Element ? event.target.closest('tr[data-row-index]') : null;
    if (!tr) return;
    event.preventDefault();
    const index = Number(tr.dataset.rowIndex);
    if (Number.isFinite(index) && currentRows[index]) onRowClick(currentRows[index], index);
  });

  renderHead();
  renderBody();

  return {
    el: wrap,
    setRows(nextRows) {
      currentRows = Array.isArray(nextRows) ? nextRows : [];
      renderBody();
    },
    getRows: () => currentRows,
    setLoading(next) {
      currentLoading = !!next;
      renderBody();
    },
  };
}
