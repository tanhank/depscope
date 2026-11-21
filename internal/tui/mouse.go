package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/janghotan/depscope/internal/i18n"
)

// handleMouse 处理鼠标滚轮滚动与左键点击交互。
func (m *Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	// 1. 处理鼠标滚轮滚动
	if msg.Button == tea.MouseButtonWheelUp {
		m.scrollBy(-3)
		return m, nil
	}
	if msg.Button == tea.MouseButtonWheelDown {
		m.scrollBy(3)
		return m, nil
	}

	// 仅响应鼠标左键按下事件
	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return m, nil
	}

	// 2. 点击顶部页眉 (Y <= 1) 返回主菜单或上级视图
	if msg.Y <= 1 {
		switch m.screen {
		case screenReport:
			if m.reportView != viewOverview {
				m.reportView = viewOverview
				m.scroll = 0
				return m, nil
			}
			return m, m.returnToMenu()
		case screenPackageForm, screenProjectForm, screenCompareForm, screenInventoryConfirm, screenDemoPicker, screenSettings, screenComparison:
			return m, m.returnToMenu()
		case screenHelp:
			m.screen = m.helpReturn
			m.scroll = 0
			return m, nil
		case screenError:
			return m, m.returnToMenu()
		}
		return m, nil
	}

	// 3. 点击底部提示栏 (Y >= m.height - 1)
	if msg.Y >= m.height-1 {
		switch m.screen {
		case screenReport:
			m.cycleReportView(1)
			return m, nil
		case screenHelp:
			m.screen = m.helpReturn
			m.scroll = 0
			return m, nil
		case screenError:
			return m, m.returnToMenu()
		}
	}

	// 4. 计算内容区域相对行号 (页眉固定占 2 行)
	contentY := msg.Y - 2 + m.scroll
	if contentY < 0 {
		return m, nil
	}

	switch m.screen {
	case screenMenu:
		return m.handleMenuMouseClick(contentY)
	case screenDemoPicker:
		return m.handleDemoMouseClick(contentY)
	case screenSettings:
		return m.handleSettingsMouseClick(contentY)
	case screenPackageForm, screenProjectForm, screenCompareForm:
		return m.handleFormMouseClick(contentY)
	case screenInventoryConfirm:
		return m, m.startScan(m.beginInventoryScan())
	case screenReport:
		return m.handleReportMouseClick(contentY)
	case screenHelp:
		m.screen = m.helpReturn
		m.scroll = 0
		return m, nil
	case screenError:
		return m, m.returnToMenu()
	}

	return m, nil
}

// handleMenuMouseClick 处理主菜单界面的点击。
func (m *Model) handleMenuMouseClick(contentY int) (tea.Model, tea.Cmd) {
	width := m.contentWidth()
	height := m.contentHeight()
	size := selectLogoSize(width, height)
	logoLines := len(strings.Split(m.brandedLogo(size), "\n"))

	// 菜单项目起始行：Logo + 标语 (2行) + "ACTIONS" 标题与空白 (2行)
	startMenuY := logoLines + 2 + 2
	if contentY >= startMenuY && contentY < startMenuY+len(menuEntries) {
		index := contentY - startMenuY
		m.menuIndex = index
		return m, m.openScreen(menuEntries[index].target)
	}
	return m, nil
}

// handleDemoMouseClick 处理演示场景选择界面的点击。
func (m *Model) handleDemoMouseClick(contentY int) (tea.Model, tea.Cmd) {
	if len(m.demos) == 0 {
		return m, nil
	}

	width := m.contentWidth()
	introLines := len(wrapPlain(m.text(i18n.KeyDemoIntro), width))
	startDemoY := 1 + 1 + introLines + 2 // 标题 + 空行 + 简介 + 2空行

	currentY := startDemoY
	for index, scenario := range m.demos {
		descLines := len(wrapPlain(m.demoDescription(scenario), width-10))
		scenarioHeight := 1 + descLines
		if contentY >= currentY && contentY < currentY+scenarioHeight {
			m.demoAt = index
			return m, m.startScan(m.beginDemoScan(scenario.ID))
		}
		currentY += scenarioHeight
	}
	return m, nil
}

// handleSettingsMouseClick 处理设置界面的点击。
func (m *Model) handleSettingsMouseClick(contentY int) (tea.Model, tea.Cmd) {
	width := m.contentWidth()
	introLines := len(wrapPlain(m.text(i18n.KeySettingsIntro), width))
	startSettingsY := 1 + 1 + introLines + 2

	if contentY >= startSettingsY && contentY < startSettingsY+5 {
		row := contentY - startSettingsY
		m.settingsAt = row
		return m, m.changeSetting(false)
	}
	return m, nil
}

// handleFormMouseClick 处理表单界面的点击。
func (m *Model) handleFormMouseClick(contentY int) (tea.Model, tea.Cmd) {
	// 如果是双输入框的对比表单
	if m.screen == screenCompareForm && len(m.formFields) >= 2 {
		if contentY >= 6 && contentY <= 7 {
			m.formIndex = 0
			return m, nil
		}
		if contentY >= 8 && contentY <= 10 {
			m.formIndex = 1
			return m, nil
		}
		if contentY > 10 {
			return m.submitForm()
		}
	} else {
		// 单输入框表单
		if contentY > 6 {
			return m.submitForm()
		}
	}
	return m, nil
}

// handleReportMouseClick 处理扫描报告界面的点击。
func (m *Model) handleReportMouseClick(contentY int) (tea.Model, tea.Cmd) {
	if m.document == nil {
		return m, nil
	}

	// 点击顶部区域 (前 3 行) 循环切换报告分节视图
	if contentY <= 3 {
		m.cycleReportView(1)
		return m, nil
	}

	// 在概览视图中，若点击漏洞列表区域，打开对应漏洞详情
	if m.reportView == viewOverview && len(m.document.Findings) > 0 {
		findingBaseY := 8 // 概览中漏洞条目通常起始位置
		if contentY >= findingBaseY {
			idx := (contentY - findingBaseY) / 2
			if idx >= 0 && idx < len(m.document.Findings) {
				m.findingIndex = idx
				m.reportView = viewFinding
				m.scroll = 0
				return m, nil
			}
		}
	} else if m.reportView == viewFinding {
		// 在详情页点击可返回概览
		if contentY <= 5 {
			m.reportView = viewOverview
			m.scroll = 0
			return m, nil
		}
	}

	return m, nil
}
