package model

type User struct {
	ID          string `json:"id"           bson:"_id"`
	Account     string `json:"account"      bson:"account"`
	SiteID      string `json:"siteId"       bson:"siteId"`
	SectID      string `json:"sectId"       bson:"sectId"`
	SectName    string `json:"sectName"     bson:"sectName"`
	SectTCName  string `json:"sectTCName"   bson:"sectTCName"`
	DeptID      string `json:"deptId"       bson:"deptId"`
	DeptName    string `json:"deptName"     bson:"deptName"`
	DeptTCName  string `json:"deptTCName"   bson:"deptTCName"`
	EngName     string `json:"engName"      bson:"engName"`
	ChineseName string `json:"chineseName"  bson:"chineseName"`
	EmployeeID  string `json:"employeeId"   bson:"employeeId"`
}
