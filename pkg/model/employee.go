package model

// Employee holds employee data looked up from the employee MongoDB collection.
type Employee struct {
	ID                    string   `bson:"_id" json:"_id"`
	AccountName           string   `bson:"accountName" json:"accountName"`
	Company               string   `bson:"company,omitempty" json:"company,omitempty"`
	Dept1Description      string   `bson:"dept1Description,omitempty" json:"dept1Description,omitempty"`
	Dept1ID               string   `bson:"dept1Id,omitempty" json:"dept1Id,omitempty"`
	Dept1Name             string   `bson:"dept1Name,omitempty" json:"dept1Name,omitempty"`
	Dept1TCName           string   `bson:"dept1TCName,omitempty" json:"dept1TCName,omitempty"`
	DeptDescription       string   `bson:"deptDescription,omitempty" json:"deptDescription,omitempty"`
	DeptID                string   `bson:"deptId,omitempty" json:"deptId,omitempty"`
	DeptName              string   `bson:"deptName,omitempty" json:"deptName,omitempty"`
	DeptTCName            string   `bson:"deptTCName,omitempty" json:"deptTCName,omitempty"`
	DivTCName             string   `bson:"divTCName,omitempty" json:"divTCName,omitempty"`
	Division1Description  string   `bson:"division1Description,omitempty" json:"division1Description,omitempty"`
	Division1ID           string   `bson:"division1Id,omitempty" json:"division1Id,omitempty"`
	Division1Name         string   `bson:"division1Name,omitempty" json:"division1Name,omitempty"`
	Division1TCName       string   `bson:"division1TCName,omitempty" json:"division1TCName,omitempty"`
	DivisionDescription   string   `bson:"divisionDescription,omitempty" json:"divisionDescription,omitempty"`
	DivisionID            string   `bson:"divisionId,omitempty" json:"divisionId,omitempty"`
	DivisionName          string   `bson:"divisionName,omitempty" json:"divisionName,omitempty"`
	EmployeeCategory      string   `bson:"employeeCategory" json:"employeeCategory"`
	EmployeeID            string   `bson:"employeeId" json:"employeeId"`
	EmployeeRoom          string   `bson:"employeeRoom" json:"employeeRoom"`
	EnglishName           string   `bson:"engName" json:"engName"`
	Function1Description  string   `bson:"function1Description,omitempty" json:"function1Description,omitempty"`
	Function1ID           string   `bson:"function1Id,omitempty" json:"function1Id,omitempty"`
	Function1Name         string   `bson:"function1Name,omitempty" json:"function1Name,omitempty"`
	Function1TCName       string   `bson:"function1TCName,omitempty" json:"function1TCName,omitempty"`
	FunctionDescription   string   `bson:"functionDescription,omitempty" json:"functionDescription,omitempty"`
	FunctionID            string   `bson:"functionId,omitempty" json:"functionId,omitempty"`
	FunctionName          string   `bson:"functionName,omitempty" json:"functionName,omitempty"`
	FunctionTCName        string   `bson:"functionTCName,omitempty" json:"functionTCName,omitempty"`
	SiteID                string   `bson:"siteId" json:"siteId"`
	JOSLocationURL        string   `bson:"josLocationURL" json:"josLocationURL"`
	Location              string   `bson:"location" json:"location"`
	LocationURL           string   `bson:"locationURL" json:"locationURL"`
	Mail                  string   `bson:"mail" json:"mail"`
	ManagedOrgIDs         []string `bson:"managedOrgIds,omitempty" json:"managedOrgIds,omitempty"`
	Name                  string   `bson:"name" json:"name"`
	Orgs                  []Org    `bson:"orgs" json:"orgs"`
	OrgCode               string   `bson:"-" json:"orgCode,omitempty"`
	Phone                 string   `bson:"phone" json:"phone"`
	Phones                []string `bson:"phones" json:"phones"`
	RosterCode            string   `bson:"rosterCode" json:"rosterCode"`
	Sect1Description      string   `bson:"sect1Description,omitempty" json:"sect1Description,omitempty"`
	Sect1ID               string   `bson:"sect1Id,omitempty" json:"sect1Id,omitempty"`
	Sect1Name             string   `bson:"sect1Name,omitempty" json:"sect1Name,omitempty"`
	Sect1TCName           string   `bson:"sect1TCName,omitempty" json:"sect1TCName,omitempty"`
	SectDescription       string   `bson:"sectDescription,omitempty" json:"sectDescription,omitempty"`
	SectID                string   `bson:"sectId,omitempty" json:"sectId,omitempty"`
	SectName              string   `bson:"sectName,omitempty" json:"sectName,omitempty"`
	SectTCName            string   `bson:"sectTCName,omitempty" json:"sectTCName,omitempty"`
	ShiftCode             string   `bson:"shiftCode" json:"shiftCode"`
	ShiftCodeDescEn       string   `bson:"shiftCodeDescEn" json:"shiftCodeDescEn"`
	ShiftCodeDescZh       string   `bson:"shiftCodeDescZh" json:"shiftCodeDescZh"`
	ShiftEndTime          string   `bson:"shiftEndTime" json:"shiftEndTime"`
	ShiftStartTime        string   `bson:"shiftStartTime" json:"shiftStartTime"`
	Status                string   `bson:"status,omitempty" json:"status,omitempty"`
	SupervisorAccountName string   `bson:"supervisorAccountName" json:"supervisorAccountName"`
	SupervisorID          string   `bson:"supervisorId" json:"supervisorId"`
	SupervisorName        string   `bson:"supervisorName" json:"supervisorName"`
	SupervisorEngName     string   `bson:"supervisorEngName" json:"supervisorEngName"`
	SupervisorPhone       string   `bson:"supervisorPhone" json:"supervisorPhone"`
	SupervisorPhones      []string `bson:"supervisorPhones" json:"supervisorPhones"`
}

// OrgType identifies an organisational unit's level in the hierarchy.
type OrgType string

const (
	OrgTypeSection1    OrgType = "section1"
	OrgTypeSection     OrgType = "section"
	OrgTypeDepartment1 OrgType = "department1"
	OrgTypeDepartment  OrgType = "department"
	OrgTypeDivision1   OrgType = "division1"
	OrgTypeDivision    OrgType = "division"
	OrgTypeFunction1   OrgType = "function1"
	OrgTypeFunction    OrgType = "function"
)

// Org is one entry in an Employee's organisational hierarchy.
type Org struct {
	ID          string  `bson:"id" json:"id"`
	Description string  `bson:"description" json:"description"`
	Name        string  `bson:"name" json:"name"`
	TCName      string  `bson:"tcName" json:"tcName"`
	Type        OrgType `bson:"type" json:"type"`
}
